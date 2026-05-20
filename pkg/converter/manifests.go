package converter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	c8dconverter "github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ManifestsConverter generates an image index that contains both legacy and
// EROFS manifests for the selected platforms.
type ManifestsConverter struct {
	cs         content.Store
	docker2oci bool
	platformMC platforms.MatchComparer
	erofsOpts  []Option
}

func NewManifestsConverter(cs content.Store, platformMC platforms.MatchComparer, docker2oci bool, erofsOpts ...Option) *ManifestsConverter {
	return &ManifestsConverter{
		cs:         cs,
		docker2oci: docker2oci,
		platformMC: platformMC,
		erofsOpts:  erofsOpts,
	}
}

func (d *ManifestsConverter) Convert(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	switch desc.MediaType {
	case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
		return d.convertIndex(ctx, desc)
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		return d.convertManifestToDualIndex(ctx, desc)
	default:
		return nil, nil
	}
}

func (d *ManifestsConverter) convertIndex(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	info, err := d.cs.Info(ctx, desc.Digest)
	if err != nil {
		return nil, err
	}
	index, err := readJSON[ocispec.Index](ctx, d.cs, desc)
	if err != nil {
		return nil, err
	}

	var newManifests []ocispec.Descriptor
	var newLabels = make(map[string]string)
	for k, v := range info.Labels {
		if !strings.HasPrefix(k, "containerd.io/gc.ref.content.") {
			newLabels[k] = v
		}
	}

	for _, m := range index.Manifests {
		if m.Platform != nil && !d.platformMC.Match(*m.Platform) {
			continue
		}

		manifest, err := readJSON[ocispec.Manifest](ctx, d.cs, m)
		if err != nil {
			return nil, err
		}
		if !shouldConvertManifest(m, manifest) {
			newLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", len(newManifests))] = m.Digest.String()
			newManifests = append(newManifests, m)
			continue
		}

		legacyDesc, erofsDesc, err := d.convertSingleManifest(ctx, m, m.Platform)
		if err != nil {
			return nil, err
		}

		newLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", len(newManifests))] = legacyDesc.Digest.String()
		newLabels[fmt.Sprintf("containerd.io/gc.ref.content.m.%d", len(newManifests)+1)] = erofsDesc.Digest.String()
		newManifests = append(newManifests, *legacyDesc, *erofsDesc)
	}

	newIndex := ocispec.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageIndex,
		Manifests:   newManifests,
		Annotations: index.Annotations,
	}

	return writeJSON(ctx, d.cs, &newIndex, desc, newLabels)
}

func (d *ManifestsConverter) convertManifestToDualIndex(ctx context.Context, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	manifest, err := readJSON[ocispec.Manifest](ctx, d.cs, desc)
	if err != nil {
		return nil, err
	}
	if !shouldConvertManifest(desc, manifest) {
		return nil, nil
	}

	legacyDesc, erofsDesc, err := d.convertSingleManifest(ctx, desc, nil)
	if err != nil {
		return nil, err
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{*legacyDesc, *erofsDesc},
	}

	labels := map[string]string{
		"containerd.io/gc.ref.content.m.0": legacyDesc.Digest.String(),
		"containerd.io/gc.ref.content.m.1": erofsDesc.Digest.String(),
	}

	return writeJSON(ctx, d.cs, &index, desc, labels)
}

func shouldConvertManifest(desc ocispec.Descriptor, manifest *ocispec.Manifest) bool {
	if manifest == nil {
		return false
	}
	if manifest.ArtifactType != "" {
		return false
	}

	switch desc.MediaType {
	case images.MediaTypeDockerSchema2Manifest:
		return true
	case ocispec.MediaTypeImageManifest:
		switch manifest.Config.MediaType {
		case ocispec.MediaTypeImageConfig, images.MediaTypeDockerSchema2Config:
			return true
		}
	}

	return false
}

func (d *ManifestsConverter) convertSingleManifest(ctx context.Context, legacyManifestDesc ocispec.Descriptor, indexPlatform *ocispec.Platform) (*ocispec.Descriptor, *ocispec.Descriptor, error) {
	legacyManifest, legacyManifestDesc, legacyConfigDesc, err := d.loadLegacyManifest(ctx, legacyManifestDesc)
	if err != nil {
		return nil, nil, err
	}

	// Convert layers to EROFS
	erofsLayers, diffIDs, err := d.convertLayersToEROFS(ctx, legacyManifest)
	if err != nil {
		return nil, nil, err
	}

	// Create EROFS config
	config, err := readJSON[ocispec.Image](ctx, d.cs, legacyManifest.Config)
	if err != nil {
		return nil, nil, err
	}
	var configPlatform *ocispec.Platform
	if config != nil {
		configPlatform = &ocispec.Platform{
			Architecture: config.Architecture,
			OS:           config.OS,
			OSVersion:    config.OSVersion,
			OSFeatures:   slices.Clone(config.OSFeatures),
			Variant:      config.Variant,
		}
	}
	config.RootFS.DiffIDs = diffIDs
	config.OSFeatures = appendOSFeature(config.OSFeatures, "erofs")
	erofsConfigLabels := map[string]string{}
	erofsConfigDesc, err := writeJSON(ctx, d.cs, config, legacyConfigDesc, erofsConfigLabels)
	if err != nil {
		return nil, nil, err
	}

	// Build GC labels for EROFS manifest
	erofsManifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": erofsConfigDesc.Digest.String(),
	}
	for i, layer := range erofsLayers {
		erofsManifestLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = layer.Digest.String()
	}

	// Create EROFS manifest
	erofsManifest := ocispec.Manifest{
		Versioned:   legacyManifest.Versioned,
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      *erofsConfigDesc,
		Layers:      erofsLayers,
		Annotations: legacyManifest.Annotations,
	}
	erofsManifestDesc, err := writeJSON(ctx, d.cs, &erofsManifest, legacyManifestDesc, erofsManifestLabels)
	if err != nil {
		return nil, nil, err
	}

	// Create new legacy manifest with annotation
	newLegacyManifest := *legacyManifest
	newLegacyManifest.Config = legacyConfigDesc
	legacyManifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": legacyManifest.Config.Digest.String(),
	}
	if len(newLegacyManifest.Layers) > 0 {
		newLegacyManifest.Layers = make([]ocispec.Descriptor, len(legacyManifest.Layers))
		copy(newLegacyManifest.Layers, legacyManifest.Layers)
		if newLegacyManifest.Layers[0].Annotations == nil {
			newLegacyManifest.Layers[0].Annotations = make(map[string]string)
		}
		newLegacyManifest.Layers[0].Annotations[EROFSManifestAnnotation] = erofsManifestDesc.Digest.String()

		// Build GC labels for legacy manifest
		for i, layer := range newLegacyManifest.Layers {
			legacyManifestLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = layer.Digest.String()
		}
	}

	updatedLegacyManifestDesc, err := writeJSON(ctx, d.cs, &newLegacyManifest, legacyManifestDesc, legacyManifestLabels)
	if err != nil {
		return nil, nil, err
	}

	descriptorPlatform := indexPlatform
	if descriptorPlatform == nil {
		descriptorPlatform = configPlatform
	}
	updatedLegacyManifestDesc.Platform = clonePlatform(descriptorPlatform)
	erofsManifestDesc.Platform = clonePlatform(descriptorPlatform)
	if erofsManifestDesc.Platform != nil {
		erofsManifestDesc.Platform.OSFeatures = appendOSFeature(erofsManifestDesc.Platform.OSFeatures, "erofs")
	}

	return updatedLegacyManifestDesc, erofsManifestDesc, nil
}

func (d *ManifestsConverter) loadLegacyManifest(ctx context.Context, legacyManifestDesc ocispec.Descriptor) (*ocispec.Manifest, ocispec.Descriptor, ocispec.Descriptor, error) {
	legacyManifest, err := readJSON[ocispec.Manifest](ctx, d.cs, legacyManifestDesc)
	if err != nil {
		return nil, ocispec.Descriptor{}, ocispec.Descriptor{}, err
	}
	if len(legacyManifest.Layers) == 0 {
		return nil, ocispec.Descriptor{}, ocispec.Descriptor{}, fmt.Errorf("manifest %s has no layers, cannot convert to EROFS", legacyManifestDesc.Digest)
	}
	legacyConfigDesc := legacyManifest.Config
	if d.docker2oci {
		legacyManifestDesc.MediaType = c8dconverter.ConvertDockerMediaTypeToOCI(legacyManifestDesc.MediaType)
		legacyConfigDesc.MediaType = c8dconverter.ConvertDockerMediaTypeToOCI(legacyConfigDesc.MediaType)
		if images.IsDockerType(legacyManifest.MediaType) {
			legacyManifest.MediaType = c8dconverter.ConvertDockerMediaTypeToOCI(legacyManifest.MediaType)
		}
	}
	return legacyManifest, legacyManifestDesc, legacyConfigDesc, nil
}

func (d *ManifestsConverter) convertLayersToEROFS(ctx context.Context, legacyManifest *ocispec.Manifest) ([]ocispec.Descriptor, []digest.Digest, error) {
	layerConvertFunc := LayerConvertFunc(d.erofsOpts...)
	var erofsLayers []ocispec.Descriptor
	var diffIDs []digest.Digest

	for i, layer := range legacyManifest.Layers {
		newLayer, err := layerConvertFunc(ctx, d.cs, layer)
		if err != nil {
			return nil, nil, err
		}
		if newLayer == nil {
			newLayer = &layer
		}
		if d.docker2oci {
			legacyManifest.Layers[i].MediaType = c8dconverter.ConvertDockerMediaTypeToOCI(layer.MediaType)
		}
		erofsLayers = append(erofsLayers, *newLayer)

		info, err := d.cs.Info(ctx, newLayer.Digest)
		if err != nil {
			return nil, nil, err
		}
		diffID, ok := info.Labels[labels.LabelUncompressed]
		if !ok || diffID == "" {
			return nil, nil, fmt.Errorf("layer %s missing %s label", newLayer.Digest, labels.LabelUncompressed)
		}
		diffIDs = append(diffIDs, digest.Digest(diffID))
	}

	return erofsLayers, diffIDs, nil
}

func clonePlatform(platform *ocispec.Platform) *ocispec.Platform {
	if platform == nil {
		return nil
	}
	cloned := *platform
	cloned.OSFeatures = slices.Clone(platform.OSFeatures)
	return &cloned
}

func appendOSFeature(features []string, feature string) []string {
	if slices.Contains(features, feature) {
		return features
	}
	return append(features, feature)
}

func readJSON[T any](ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*T, error) {
	data, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	var obj T
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func writeJSON(ctx context.Context, cs content.Store, obj interface{}, refDesc ocispec.Descriptor, labels map[string]string) (*ocispec.Descriptor, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}

	dgst := digest.FromBytes(data)
	ref := fmt.Sprintf("erofs-dual-%s", dgst)

	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
	} else {
		if err := w.Truncate(0); err != nil {
			w.Close()
			return nil, err
		}
		if err := content.Copy(ctx, w, bytes.NewReader(data), int64(len(data)), dgst, content.WithLabels(labels)); err != nil && !errdefs.IsAlreadyExists(err) {
			w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	mediaType := refDesc.MediaType
	if _, ok := obj.(*ocispec.Index); ok {
		mediaType = ocispec.MediaTypeImageIndex
	} else if _, ok := obj.(*ocispec.Manifest); ok {
		mediaType = ocispec.MediaTypeImageManifest
	}

	result := refDesc
	result.MediaType = mediaType
	result.Digest = dgst
	result.Size = int64(len(data))
	return &result, nil
}
