package converter

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type options struct {
	uuid          string
	compressors   string
	extraMkfsOpts string
}

type Option func(o *options) error

func WithCompressors(compressors string) Option {
	return func(o *options) error {
		o.compressors = compressors
		return nil
	}
}

func WithUUID(uuid string) Option {
	return func(o *options) error {
		o.uuid = uuid
		return nil
	}
}

func WithExtraMkfsOption(extraMkfsOpts string) Option {
	return func(o *options) error {
		o.extraMkfsOpts = extraMkfsOpts
		return nil
	}
}

func convertTarErofs(ctx context.Context, r io.Reader, layerPath string, mkfsExtraOpts []string) error {
	args := append([]string{"--tar=f", "--aufs", "--quiet"}, mkfsExtraOpts...)
	args = append(args, layerPath)
	cmd := exec.CommandContext(ctx, "mkfs.erofs", args...)
	cmd.Stdin = r
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.erofs %s failed: %s: %w", cmd.Args, out, err)
	}
	log.G(ctx).Debugf("running %s %s %v", cmd.Path, cmd.Args, string(out))
	return nil
}

var hasMkfs = false
var hasMkfsOnce sync.Once

func hasMkfsErofs() bool {
	hasMkfsOnce.Do(func() {
		_, err := exec.LookPath("mkfs.erofs")
		if err != nil {
			return
		}
		hasMkfs = true
	})
	return hasMkfs
}

func LayerConvertFunc(opt ...Option) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		var opts options

		for _, o := range opt {
			if err := o(&opts); err != nil {
				return nil, err
			}
		}

		if !hasMkfsErofs() {
			return nil, errdefs.ErrNotImplemented
		}
		if !images.IsLayerType(desc.MediaType) {
			// No conversion. No need to return an error here.
			return nil, nil
		}
		uncompressedDesc := &desc
		// We need to uncompress the archive first
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompressedDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, err
			}
			if uncompressedDesc == nil {
				return nil, fmt.Errorf("unexpectedly got the same blob after compression (%s, %q)", desc.Digest, desc.MediaType)
			}
			log.G(ctx).Debugf("uncompressed %s into %s", desc.Digest, uncompressedDesc.Digest)
		}

		info, err := cs.Info(ctx, desc.Digest)
		labelz := info.Labels
		if labelz == nil {
			labelz = make(map[string]string)
		}

		ra, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, err
		}
		defer ra.Close()
		sr := io.NewSectionReader(ra, 0, uncompressedDesc.Size)

		blob, err := ioutil.TempFile("", "erofs-layer-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(blob.Name()) // clean up
		defer blob.Close()

		var extraopts []string

		if opts.uuid != "" {
			extraopts = append(extraopts, opts.uuid)
		} else {
			extraopts = append(extraopts, []string{"-U", "fead9a88-fd26-578a-a655-9cbddcb89e76"}...)
		}
		if opts.compressors != "" {
			extraopts = append(extraopts, []string{"-z", opts.compressors}...)
			extraopts = append(extraopts, []string{"-C", "65536"}...)
		}
		if opts.extraMkfsOpts != "" {
			extraopts = append(extraopts, opts.extraMkfsOpts)
		}

		err = convertTarErofs(ctx, sr, blob.Name(), extraopts)
		if err != nil {
			return nil, err
		}

		ref := fmt.Sprintf("convert-erofs-from-%s", desc.Digest)
		w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
		if err != nil {
			return nil, err
		}
		defer w.Close()

		// Reset the writing position
		// Old writer possibly remains without aborted
		// (e.g. conversion interrupted by a signal)
		if err := w.Truncate(0); err != nil {
			return nil, err
		}

		n, err := io.Copy(w, blob)
		if err != nil {
			return nil, err
		}
		if err := blob.Close(); err != nil {
			return nil, err
		}

		// update diffID label
		labelz[labels.LabelUncompressed] = w.Digest().String()
		if err = w.Commit(ctx, n, "", content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}

		newDesc := desc
		newDesc.MediaType = "application/vnd.erofs"
		newDesc.Digest = w.Digest()
		newDesc.Size = n
		return &newDesc, nil
	}
}
