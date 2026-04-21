package converter

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type memoryLabelStore struct {
	mu     sync.Mutex
	labels map[digest.Digest]map[string]string
}

func newMemoryLabelStore() local.LabelStore {
	return &memoryLabelStore{
		labels: map[digest.Digest]map[string]string{},
	}
}

func (m *memoryLabelStore) Get(d digest.Digest) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if labels, ok := m.labels[d]; ok {
		cloned := make(map[string]string, len(labels))
		for k, v := range labels {
			cloned[k] = v
		}
		return cloned, nil
	}
	return nil, nil
}

func (m *memoryLabelStore) Set(d digest.Digest, labels map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cloned := make(map[string]string, len(labels))
	for k, v := range labels {
		cloned[k] = v
	}
	m.labels[d] = cloned
	return nil
}

func (m *memoryLabelStore) Update(d digest.Digest, update map[string]string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	labels, ok := m.labels[d]
	if !ok {
		labels = map[string]string{}
	}
	for k, v := range update {
		if v == "" {
			delete(labels, k)
		} else {
			labels[k] = v
		}
	}
	cloned := make(map[string]string, len(labels))
	for k, v := range labels {
		cloned[k] = v
	}
	m.labels[d] = cloned
	return cloned, nil
}

func requireMkfsErofs(t *testing.T) {
	t.Helper()
	if hasMkfsErofs() {
		return
	}
	t.Log("warning: mkfs.erofs not found; skipping conversion-dependent test")
	t.Skip("mkfs.erofs is required for this test")
}

func TestShouldConvertManifest(t *testing.T) {
	tests := []struct {
		name     string
		desc     ocispec.Descriptor
		manifest *ocispec.Manifest
		want     bool
	}{
		{
			name: "docker manifest converts",
			desc: ocispec.Descriptor{MediaType: images.MediaTypeDockerSchema2Manifest},
			manifest: &ocispec.Manifest{
				Config: ocispec.Descriptor{MediaType: images.MediaTypeDockerSchema2Config},
			},
			want: true,
		},
		{
			name: "oci image manifest converts",
			desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest},
			manifest: &ocispec.Manifest{
				Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig},
			},
			want: true,
		},
		{
			name: "oci artifact manifest passes through",
			desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest},
			manifest: &ocispec.Manifest{
				ArtifactType: "application/spdx+json",
				Config:       ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON},
			},
			want: false,
		},
		{
			name: "oci empty config passes through",
			desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest},
			manifest: &ocispec.Manifest{
				Config: ocispec.Descriptor{MediaType: ocispec.MediaTypeEmptyJSON},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldConvertManifest(tc.desc, tc.manifest); got != tc.want {
				t.Fatalf("shouldConvertManifest() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConvertManifestToDualIndex(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newLabeledTestStore(t)
	manifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "amd64", OSFeatures: []string{"sse4"}},
	})

	converter := NewManifestsConverter(cs, platforms.All, false)
	indexDesc, err := converter.convertManifestToDualIndex(ctx, manifestDesc)
	if err != nil {
		t.Fatalf("convertManifestToDualIndex() error = %v", err)
	}
	if indexDesc == nil {
		t.Fatal("convertManifestToDualIndex() returned nil descriptor")
	}
	if indexDesc.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("index media type = %q, want %q", indexDesc.MediaType, ocispec.MediaTypeImageIndex)
	}

	index := readJSONForTest[ocispec.Index](t, ctx, cs, *indexDesc)
	if len(index.Manifests) != 2 {
		t.Fatalf("dual index manifests = %d, want 2", len(index.Manifests))
	}

	indexLabels := infoLabelsForTest(t, ctx, cs, indexDesc.Digest)
	if indexLabels["containerd.io/gc.ref.content.m.0"] != index.Manifests[0].Digest.String() {
		t.Fatalf("index gc label m.0 = %q, want %q", indexLabels["containerd.io/gc.ref.content.m.0"], index.Manifests[0].Digest)
	}
	if indexLabels["containerd.io/gc.ref.content.m.1"] != index.Manifests[1].Digest.String() {
		t.Fatalf("index gc label m.1 = %q, want %q", indexLabels["containerd.io/gc.ref.content.m.1"], index.Manifests[1].Digest)
	}

	legacyManifest := readJSONForTest[ocispec.Manifest](t, ctx, cs, index.Manifests[0])
	erofsManifest := readJSONForTest[ocispec.Manifest](t, ctx, cs, index.Manifests[1])
	legacyConfig := readJSONForTest[ocispec.Image](t, ctx, cs, legacyManifest.Config)
	erofsConfig := readJSONForTest[ocispec.Image](t, ctx, cs, erofsManifest.Config)

	if got := legacyManifest.Layers[0].Annotations[EROFSManifestAnnotation]; got != index.Manifests[1].Digest.String() {
		t.Fatalf("legacy manifest annotation = %q, want %q", got, index.Manifests[1].Digest)
	}
	if erofsManifest.Config.Digest == legacyManifest.Config.Digest {
		t.Fatal("erofs config digest unexpectedly matches legacy config digest")
	}
	if erofsManifest.Layers[0].MediaType != EROFSLayerMediaType {
		t.Fatalf("erofs layer media type = %q, want %q", erofsManifest.Layers[0].MediaType, EROFSLayerMediaType)
	}
	if !reflect.DeepEqual(legacyConfig.OSFeatures, []string{"sse4"}) {
		t.Fatalf("legacy config os.features = %#v, want %#v", legacyConfig.OSFeatures, []string{"sse4"})
	}
	if !reflect.DeepEqual(erofsConfig.OSFeatures, []string{"sse4", "erofs"}) {
		t.Fatalf("erofs config os.features = %#v, want %#v", erofsConfig.OSFeatures, []string{"sse4", "erofs"})
	}

	wantPlatform := &ocispec.Platform{OS: "linux", Architecture: "amd64", OSFeatures: []string{"sse4"}}
	if !reflect.DeepEqual(index.Manifests[0].Platform, wantPlatform) {
		t.Fatalf("legacy platform = %#v, want %#v", index.Manifests[0].Platform, wantPlatform)
	}
	wantEROFSPlatform := &ocispec.Platform{OS: "linux", Architecture: "amd64", OSFeatures: []string{"sse4", "erofs"}}
	if !reflect.DeepEqual(index.Manifests[1].Platform, wantEROFSPlatform) {
		t.Fatalf("erofs platform = %#v, want %#v", index.Manifests[1].Platform, wantEROFSPlatform)
	}

	legacyLabels := infoLabelsForTest(t, ctx, cs, index.Manifests[0].Digest)
	if legacyLabels["containerd.io/gc.ref.content.config"] != legacyManifest.Config.Digest.String() {
		t.Fatalf("legacy config gc label = %q, want %q", legacyLabels["containerd.io/gc.ref.content.config"], legacyManifest.Config.Digest)
	}
	if legacyLabels["containerd.io/gc.ref.content.l.0"] != legacyManifest.Layers[0].Digest.String() {
		t.Fatalf("legacy layer gc label = %q, want %q", legacyLabels["containerd.io/gc.ref.content.l.0"], legacyManifest.Layers[0].Digest)
	}

	erofsLabels := infoLabelsForTest(t, ctx, cs, index.Manifests[1].Digest)
	if erofsLabels["containerd.io/gc.ref.content.config"] != erofsManifest.Config.Digest.String() {
		t.Fatalf("erofs config gc label = %q, want %q", erofsLabels["containerd.io/gc.ref.content.config"], erofsManifest.Config.Digest)
	}
	if erofsLabels["containerd.io/gc.ref.content.l.0"] != erofsManifest.Layers[0].Digest.String() {
		t.Fatalf("erofs layer gc label = %q, want %q", erofsLabels["containerd.io/gc.ref.content.l.0"], erofsManifest.Layers[0].Digest)
	}
}

func TestConvertIndexMixedImageAndArtifact(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newLabeledTestStore(t)

	imageManifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "amd64"},
	})

	artifactConfigDesc := writeRawBlob(t, ctx, cs, []byte("{}"), ocispec.MediaTypeEmptyJSON, nil)
	artifactLayerDesc := writeRawBlob(t, ctx, cs, []byte("sbom"), "application/spdx+json", nil)
	artifactManifest := ocispec.Manifest{
		Versioned:    ocispec.Manifest{}.Versioned,
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: "application/spdx+json",
		Config:       artifactConfigDesc,
		Layers:       []ocispec.Descriptor{artifactLayerDesc},
	}
	artifactManifest.Versioned.SchemaVersion = 2
	artifactManifestDesc := writeJSONBlob(t, ctx, cs, artifactManifest, ocispec.MediaTypeImageManifest, nil)

	artifactEntry := artifactManifestDesc
	artifactEntry.Annotations = map[string]string{"org.opencontainers.image.ref.name": "sbom.spdx.json"}
	artifactEntry.URLs = []string{"https://example.invalid/sbom"}
	artifactEntry.Data = []byte("inline-metadata")
	artifactEntry.ArtifactType = "application/spdx+json"

	index := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{imageManifestDesc, artifactEntry},
	}
	index.Versioned.SchemaVersion = 2
	indexDesc := writeJSONBlob(t, ctx, cs, index, ocispec.MediaTypeImageIndex, map[string]string{
		"custom":                           "keep",
		"containerd.io/gc.ref.content.m.0": "stale-legacy",
	})
	indexDesc.Annotations = map[string]string{"descriptor-annotation": "preserve"}
	indexDesc.URLs = []string{"https://example.invalid/index"}
	indexDesc.Data = []byte("descriptor-data")
	indexDesc.ArtifactType = "application/example.index"

	converter := NewManifestsConverter(cs, platforms.All, false)
	newIndexDesc, err := converter.convertIndex(ctx, indexDesc)
	if err != nil {
		t.Fatalf("convertIndex() error = %v", err)
	}
	if newIndexDesc == nil {
		t.Fatal("convertIndex() returned nil descriptor")
	}
	if !reflect.DeepEqual(newIndexDesc.Annotations, indexDesc.Annotations) {
		t.Fatalf("descriptor annotations = %#v, want %#v", newIndexDesc.Annotations, indexDesc.Annotations)
	}
	if !reflect.DeepEqual(newIndexDesc.URLs, indexDesc.URLs) {
		t.Fatalf("descriptor URLs = %#v, want %#v", newIndexDesc.URLs, indexDesc.URLs)
	}
	if !bytes.Equal(newIndexDesc.Data, indexDesc.Data) {
		t.Fatalf("descriptor data = %q, want %q", string(newIndexDesc.Data), string(indexDesc.Data))
	}
	if newIndexDesc.ArtifactType != indexDesc.ArtifactType {
		t.Fatalf("descriptor artifact type = %q, want %q", newIndexDesc.ArtifactType, indexDesc.ArtifactType)
	}

	newIndex := readJSONForTest[ocispec.Index](t, ctx, cs, *newIndexDesc)
	if len(newIndex.Manifests) != 3 {
		t.Fatalf("new index manifests = %d, want 3", len(newIndex.Manifests))
	}
	if !reflect.DeepEqual(newIndex.Manifests[2], artifactEntry) {
		t.Fatalf("artifact entry changed:\n got: %#v\nwant: %#v", newIndex.Manifests[2], artifactEntry)
	}

	newIndexLabels := infoLabelsForTest(t, ctx, cs, newIndexDesc.Digest)
	if newIndexLabels["custom"] != "keep" {
		t.Fatalf("custom label = %q, want keep", newIndexLabels["custom"])
	}
	if newIndexLabels["containerd.io/gc.ref.content.m.0"] != newIndex.Manifests[0].Digest.String() {
		t.Fatalf("gc label m.0 = %q, want %q", newIndexLabels["containerd.io/gc.ref.content.m.0"], newIndex.Manifests[0].Digest)
	}
	if newIndexLabels["containerd.io/gc.ref.content.m.1"] != newIndex.Manifests[1].Digest.String() {
		t.Fatalf("gc label m.1 = %q, want %q", newIndexLabels["containerd.io/gc.ref.content.m.1"], newIndex.Manifests[1].Digest)
	}
	if newIndexLabels["containerd.io/gc.ref.content.m.2"] != artifactEntry.Digest.String() {
		t.Fatalf("gc label m.2 = %q, want %q", newIndexLabels["containerd.io/gc.ref.content.m.2"], artifactEntry.Digest)
	}
	if newIndexLabels["containerd.io/gc.ref.content.m.3"] != "" {
		t.Fatalf("unexpected extra gc label m.3 = %q", newIndexLabels["containerd.io/gc.ref.content.m.3"])
	}
}

func TestConvertIndexPreservesDescriptorPlatform(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newLabeledTestStore(t)

	manifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "arm"},
	})
	descriptorPlatform := &ocispec.Platform{
		OS:           "linux",
		Architecture: "arm",
		Variant:      "v7",
		OSVersion:    "custom-os-version",
		OSFeatures:   []string{"vfp"},
	}
	manifestDesc.Platform = descriptorPlatform

	index := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifestDesc},
	}
	index.Versioned.SchemaVersion = 2
	indexDesc := writeJSONBlob(t, ctx, cs, index, ocispec.MediaTypeImageIndex, nil)

	converter := NewManifestsConverter(cs, platforms.All, false)
	newIndexDesc, err := converter.convertIndex(ctx, indexDesc)
	if err != nil {
		t.Fatalf("convertIndex() error = %v", err)
	}

	newIndex := readJSONForTest[ocispec.Index](t, ctx, cs, *newIndexDesc)
	if len(newIndex.Manifests) != 2 {
		t.Fatalf("new index manifests = %d, want 2", len(newIndex.Manifests))
	}
	if !reflect.DeepEqual(newIndex.Manifests[0].Platform, descriptorPlatform) {
		t.Fatalf("legacy platform = %#v, want %#v", newIndex.Manifests[0].Platform, descriptorPlatform)
	}
	wantEROFSPlatform := clonePlatform(descriptorPlatform)
	wantEROFSPlatform.OSFeatures = append(wantEROFSPlatform.OSFeatures, "erofs")
	if !reflect.DeepEqual(newIndex.Manifests[1].Platform, wantEROFSPlatform) {
		t.Fatalf("erofs platform = %#v, want %#v", newIndex.Manifests[1].Platform, wantEROFSPlatform)
	}
}

func TestConvertIndexPlatformFilter(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newLabeledTestStore(t)

	amd64ManifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "amd64"},
	})
	arm64ManifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "arm64"},
	})

	index := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{amd64ManifestDesc, arm64ManifestDesc},
	}
	index.Versioned.SchemaVersion = 2
	indexDesc := writeJSONBlob(t, ctx, cs, index, ocispec.MediaTypeImageIndex, nil)

	converter := NewManifestsConverter(cs, platforms.OnlyStrict(ocispec.Platform{OS: "linux", Architecture: "amd64"}), false)
	newIndexDesc, err := converter.convertIndex(ctx, indexDesc)
	if err != nil {
		t.Fatalf("convertIndex() error = %v", err)
	}

	newIndex := readJSONForTest[ocispec.Index](t, ctx, cs, *newIndexDesc)
	if len(newIndex.Manifests) != 2 {
		t.Fatalf("filtered index manifests = %d, want 2", len(newIndex.Manifests))
	}
	for _, manifestDesc := range newIndex.Manifests {
		if manifestDesc.Platform == nil || manifestDesc.Platform.Architecture != "amd64" {
			t.Fatalf("manifest platform = %#v, want amd64", manifestDesc.Platform)
		}
	}
}

func TestConvertSingleManifestDockerToOCI(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newLabeledTestStore(t)
	manifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: images.MediaTypeDockerSchema2Manifest,
		configMediaType:   images.MediaTypeDockerSchema2Config,
		layerMediaType:    images.MediaTypeDockerSchema2Layer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "amd64"},
	})

	converter := NewManifestsConverter(cs, platforms.All, true)
	updatedLegacyDesc, erofsDesc, err := converter.convertSingleManifest(ctx, manifestDesc, nil)
	if err != nil {
		t.Fatalf("convertSingleManifest() error = %v", err)
	}
	if updatedLegacyDesc.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("legacy descriptor media type = %q, want %q", updatedLegacyDesc.MediaType, ocispec.MediaTypeImageManifest)
	}
	if erofsDesc.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("erofs descriptor media type = %q, want %q", erofsDesc.MediaType, ocispec.MediaTypeImageManifest)
	}

	legacyManifest := readJSONForTest[ocispec.Manifest](t, ctx, cs, *updatedLegacyDesc)
	if legacyManifest.MediaType != ocispec.MediaTypeImageManifest {
		t.Fatalf("legacy manifest media type = %q, want %q", legacyManifest.MediaType, ocispec.MediaTypeImageManifest)
	}
	if legacyManifest.Config.MediaType != ocispec.MediaTypeImageConfig {
		t.Fatalf("legacy config media type = %q, want %q", legacyManifest.Config.MediaType, ocispec.MediaTypeImageConfig)
	}
	if legacyManifest.Layers[0].MediaType != ocispec.MediaTypeImageLayer {
		t.Fatalf("legacy layer media type = %q, want %q", legacyManifest.Layers[0].MediaType, ocispec.MediaTypeImageLayer)
	}

	erofsManifest := readJSONForTest[ocispec.Manifest](t, ctx, cs, *erofsDesc)
	if erofsManifest.Config.MediaType != ocispec.MediaTypeImageConfig {
		t.Fatalf("erofs config media type = %q, want %q", erofsManifest.Config.MediaType, ocispec.MediaTypeImageConfig)
	}
}

func TestConvertSingleManifestMissingUncompressedLabel(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	cs := newUnlabeledTestStore(t)
	manifestDesc, _, _, _ := writeTestImage(t, ctx, cs, testImageSpec{
		manifestMediaType: ocispec.MediaTypeImageManifest,
		configMediaType:   ocispec.MediaTypeImageConfig,
		layerMediaType:    ocispec.MediaTypeImageLayer,
		platform:          ocispec.Platform{OS: "linux", Architecture: "amd64"},
	})

	converter := NewManifestsConverter(cs, platforms.All, false)
	_, _, err := converter.convertSingleManifest(ctx, manifestDesc, nil)
	if err == nil {
		t.Fatal("convertSingleManifest() unexpectedly succeeded without uncompressed label support")
	}
	if !strings.Contains(err.Error(), labels.LabelUncompressed) {
		t.Fatalf("error %q does not mention missing %s", err, labels.LabelUncompressed)
	}
}

type testImageSpec struct {
	manifestMediaType string
	configMediaType   string
	layerMediaType    string
	platform          ocispec.Platform
}

func newLabeledTestStore(t *testing.T) content.Store {
	t.Helper()
	root := t.TempDir()
	cs, err := local.NewLabeledStore(root, newMemoryLabelStore())
	if err != nil {
		t.Fatalf("NewLabeledStore() error = %v", err)
	}
	return cs
}

func newUnlabeledTestStore(t *testing.T) content.Store {
	t.Helper()
	root := t.TempDir()
	cs, err := local.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return cs
}

func writeTestImage(t *testing.T, ctx context.Context, cs content.Store, spec testImageSpec) (ocispec.Descriptor, ocispec.Descriptor, ocispec.Descriptor, digest.Digest) {
	t.Helper()

	layerData := tarBytesForTest(t)
	layerDesc := writeRawBlob(t, ctx, cs, layerData, spec.layerMediaType, nil)

	config := ocispec.Image{
		Platform: spec.platform,
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{layerDesc.Digest},
		},
	}
	configDesc := writeJSONBlob(t, ctx, cs, config, spec.configMediaType, nil)

	manifest := ocispec.Manifest{
		MediaType: spec.manifestMediaType,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifest.Versioned.SchemaVersion = 2
	manifestDesc := writeJSONBlob(t, ctx, cs, manifest, spec.manifestMediaType, nil)
	manifestDesc.Platform = &ocispec.Platform{OS: spec.platform.OS, Architecture: spec.platform.Architecture}

	return manifestDesc, configDesc, layerDesc, layerDesc.Digest
}

func tarBytesForTest(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte("hello erofs\n")
	hdr := &tar.Header{
		Name: "hello.txt",
		Mode: 0o644,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return buf.Bytes()
}

func writeRawBlob(t *testing.T, ctx context.Context, cs content.Store, data []byte, mediaType string, blobLabels map[string]string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := content.WriteBlob(ctx, cs, desc.Digest.String(), bytes.NewReader(data), desc, content.WithLabels(blobLabels)); err != nil {
		t.Fatalf("WriteBlob(%s) error = %v", mediaType, err)
	}
	return desc
}

func writeJSONBlob(t *testing.T, ctx context.Context, cs content.Store, obj interface{}, mediaType string, blobLabels map[string]string) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return writeRawBlob(t, ctx, cs, data, mediaType, blobLabels)
}

func readJSONForTest[T any](t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor) *T {
	t.Helper()
	obj, err := readJSON[T](ctx, cs, desc)
	if err != nil {
		t.Fatalf("readJSON(%s) error = %v", desc.Digest, err)
	}
	return obj
}

func infoLabelsForTest(t *testing.T, ctx context.Context, cs content.Store, dgst digest.Digest) map[string]string {
	t.Helper()
	info, err := cs.Info(ctx, dgst)
	if err != nil {
		t.Fatalf("Info(%s) error = %v", dgst, err)
	}
	return info.Labels
}
