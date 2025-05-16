/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package commands

import (
	gocontext "context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	convert "github.com/erofs/erofs-container-toolkit/pkg/converter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
)

// ConvertCommand converts an image
var ConvertCommand = &cli.Command{
	Name:      "convert",
	Usage:     "convert an image",
	ArgsUsage: "[flags] <source_ref> <target_ref>...",
	Description: `Convert an image format.

e.g., 'ctr-remote convert --erofs --oci example.com/foo:orig example.com/foo:erofs'

Use '--platform' to define the output platform.
When '--all-platforms' is given all images in a manifest list must be available.
`,
	Flags: []cli.Flag{
		// erofs flags
		&cli.BoolFlag{
			Name:  "erofs",
			Usage: "Convert docker or OCI layers to EROFS native layers. Should be used in conjunction with '--oci'",
		},
		&cli.StringFlag{
			Name:  "erofs-compressors",
			Usage: "Specify compression algorithm list when converting EROFS layers",
		},
		&cli.StringFlag{
			Name:  "erofs-mkfs-options",
			Usage: "Extra mkfs options applied when converting EROFS layers. (e.g. '-Efragments,dedupe')",
		},
		// generic flags
		&cli.BoolFlag{
			Name:  "uncompress",
			Usage: "Convert tar.gz layers to uncompressed tar layers",
		},
		&cli.BoolFlag{
			Name:  "oci",
			Usage: "Convert Docker media types to OCI media type",
		},
		// platform flags
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: &cli.StringSlice{},
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "Exports content from all platforms",
		},
	},
	Action: func(context *cli.Context) error {
		var convertOpts []converter.Opt
		srcRef := context.Args().Get(0)
		targetRef := context.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		var platformMC platforms.MatchComparer
		if context.Bool("all-platforms") {
			platformMC = platforms.All
		} else {
			if pss := context.StringSlice("platform"); len(pss) > 0 {
				var all []ocispec.Platform
				for _, ps := range pss {
					p, err := platforms.Parse(ps)
					if err != nil {
						return fmt.Errorf("invalid platform %q: %w", ps, err)
					}
					all = append(all, p)
				}
				platformMC = platforms.Ordered(all...)
			} else {
				platformMC = platforms.DefaultStrict()
			}
		}
		convertOpts = append(convertOpts, converter.WithPlatform(platformMC))

		var layerConvertFunc converter.ConvertFunc
		var finalize func(ctx gocontext.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)
		if context.Bool("erofs") {
			Opts := []convert.Option{
				convert.WithCompressors(context.String("erofs-compressors")),
				convert.WithExtraMkfsOption(context.String("erofs-mkfs-options")),
			}

			layerConvertFunc = convert.LayerConvertFunc(Opts...)
			if !context.Bool("oci") {
				log.L.Warn("option --erofs should be used in conjunction with --oci")
			}
			if context.Bool("uncompress") {
				return errors.New("option --erofs conflicts with --uncompress")
			}
		}

		if context.Bool("uncompress") {
			layerConvertFunc = uncompress.LayerConvertFunc
		}

		if context.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		}

		if layerConvertFunc != nil {
			convertOpts = append(convertOpts, converter.WithLayerConvertFunc(layerConvertFunc))
		}

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			// Cleanly cancel conversion
			select {
			case s := <-sigCh:
				log.G(ctx).Infof("Got %v", s)
				cancel()
			case <-ctx.Done():
			}
		}()
		newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		if finalize != nil {
			newI, err := finalize(ctx, client.ContentStore(), targetRef, &newImg.Target)
			if err != nil {
				return err
			}
			is := client.ImageService()
			_ = is.Delete(ctx, newI.Name)
			finimg, err := is.Create(ctx, *newI)
			if err != nil {
				return err
			}
			fmt.Fprintln(context.App.Writer, "extra image:", finimg.Name)
		}
		fmt.Fprintln(context.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}
