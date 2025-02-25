// Copyright 2022, 2023 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	gzip "github.com/klauspost/pgzip"
	"go.opentelemetry.io/otel"

	apkfs "github.com/chainguard-dev/go-apk/pkg/fs"
	"github.com/chainguard-dev/go-apk/pkg/tarball"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	ggcrtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/cosign/v2/pkg/oci"
	"gitlab.alpinelinux.org/alpine/go/repository"
	khash "sigs.k8s.io/release-utils/hash"

	chainguardAPK "chainguard.dev/apko/pkg/apk"
	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/apko/pkg/options"
	"chainguard.dev/apko/pkg/sbom"
	soptions "chainguard.dev/apko/pkg/sbom/options"
)

// BuildTarball takes the fully populated working directory and saves it to
// an OCI image layer tar.gz file.
func (bc *Context) BuildTarball(ctx context.Context) (string, hash.Hash, hash.Hash, int64, error) {
	ctx, span := otel.Tracer("apko").Start(ctx, "BuildTarball")
	defer span.End()

	var outfile *os.File
	var err error

	if bc.o.TarballPath != "" {
		outfile, err = os.Create(bc.o.TarballPath)
	} else {
		outfile, err = os.Create(filepath.Join(bc.o.TempDir(), bc.o.TarballFileName()))
	}
	if err != nil {
		return "", nil, nil, 0, fmt.Errorf("opening the build context tarball path failed: %w", err)
	}
	bc.o.TarballPath = outfile.Name()
	defer outfile.Close()

	// we use a general override of 0,0 for all files, but the specific overrides, that come from the installed package DB, come later
	tw, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(bc.o.SourceDateEpoch),
	)
	if err != nil {
		return "", nil, nil, 0, fmt.Errorf("failed to construct tarball build context: %w", err)
	}

	digest := sha256.New()

	buf := bufio.NewWriterSize(outfile, 1<<22)
	gzw := gzip.NewWriter(io.MultiWriter(digest, buf))

	diffid := sha256.New()

	if err := tw.WriteTar(ctx, io.MultiWriter(diffid, gzw), bc.fs); err != nil {
		return "", nil, nil, 0, fmt.Errorf("failed to generate tarball for image: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return "", nil, nil, 0, fmt.Errorf("closing gzip writer: %w", err)
	}

	if err := buf.Flush(); err != nil {
		return "", nil, nil, 0, fmt.Errorf("flushing %s: %w", outfile.Name(), err)
	}

	stat, err := outfile.Stat()
	if err != nil {
		return "", nil, nil, 0, fmt.Errorf("stat(%q): %w", outfile.Name(), err)
	}

	bc.Logger().Infof("built image layer tarball as %s", outfile.Name())
	return outfile.Name(), diffid, digest, stat.Size(), nil
}

func (bc *Context) GenerateImageSBOM(ctx context.Context, arch types.Architecture, img oci.SignedImage) ([]types.SBOM, error) {
	ctx, span := otel.Tracer("apko").Start(ctx, "GenerateImageSBOM")
	defer span.End()

	if !bc.WantSBOM() {
		bc.Logger().Warnf("skipping SBOM generation")
		return nil, nil
	}

	bde, err := bc.GetBuildDateEpoch()
	if err != nil {
		return nil, fmt.Errorf("computing build date epoch: %w", err)
	}

	s := newSBOM(bc.fs, bc.o, bc.ic, bde)

	m, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("getting %s manifest: %w", arch, err)
	}

	if len(m.Layers) != 1 {
		return nil, fmt.Errorf("unexpected layers in %s manifest: %d", arch, len(m.Layers))
	}

	if err := s.SetLayerDigest(ctx, m.Layers[0].Digest); err != nil {
		return nil, fmt.Errorf("reading layer tar: %w", err)
	}

	if err := s.ReadReleaseData(); err != nil {
		return nil, fmt.Errorf("getting os-release: %w", err)
	}

	if err := s.ReadPackageIndex(); err != nil {
		return nil, fmt.Errorf("getting installed packages from sbom: %w", err)
	}

	// Get the image digest
	h, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("getting %s image digest: %w", arch, err)
	}

	s.Options.ImageInfo.ImageDigest = h.String()
	s.Options.ImageInfo.Arch = arch

	var sboms = make([]types.SBOM, 0)
	files, err := s.Generate()
	if err != nil {
		return nil, fmt.Errorf("generating sbom: %w", err)
	}
	for _, f := range files {
		log.Printf("f = %s", f)
		sboms = append(sboms, types.SBOM{
			Path:   f,
			Format: bc.o.SBOMFormats[0],
			Arch:   arch.String(),
			Digest: h,
		})
	}
	return sboms, nil
}

func additionalTags(fsys apkfs.FullFS, o *options.Options) error {
	at, err := chainguardAPK.AdditionalTags(fsys, *o)
	if err != nil {
		return err
	}
	if at == nil {
		return nil
	}
	o.Tags = append(o.Tags, at...)
	return nil
}

func (bc *Context) buildImage(ctx context.Context) error {
	ctx, span := otel.Tracer("apko").Start(ctx, "buildImage")
	defer span.End()

	if err := bc.apk.FixateWorld(ctx, &bc.o.SourceDateEpoch); err != nil {
		return fmt.Errorf("installing apk packages: %w", err)
	}

	if err := additionalTags(bc.fs, &bc.o); err != nil {
		return fmt.Errorf("adding additional tags: %w", err)
	}

	if err := mutateAccounts(bc.fs, &bc.o, &bc.ic); err != nil {
		return fmt.Errorf("failed to mutate accounts: %w", err)
	}

	if err := mutatePaths(bc.fs, &bc.o, &bc.ic); err != nil {
		return fmt.Errorf("failed to mutate paths: %w", err)
	}

	if err := GenerateOSRelease(bc.fs, &bc.o, &bc.ic); err != nil {
		if errors.Is(err, ErrOSReleaseAlreadyPresent) {
			bc.Logger().Warnf("did not generate /etc/os-release: %v", err)
		} else {
			return fmt.Errorf("failed to generate /etc/os-release: %w", err)
		}
	}

	if err := bc.s6.WriteSupervisionTree(bc.ic.Entrypoint.Services); err != nil {
		return fmt.Errorf("failed to write supervision tree: %w", err)
	}

	// add busybox symlinks
	installed, err := bc.apk.GetInstalled()
	if err != nil {
		return fmt.Errorf("getting installed packages: %w", err)
	}

	if err := installBusyboxLinks(bc.fs, installed); err != nil {
		return err
	}

	// add ldconfig links
	if err := installLdconfigLinks(bc.fs); err != nil {
		return err
	}

	// add necessary character devices
	if err := installCharDevices(bc.fs); err != nil {
		return err
	}

	bc.Logger().Infof("finished building filesystem in %s", bc.o.WorkDir)

	return nil
}

// WriteIndex saves the index file from the given image configuration.
func (bc *Context) WriteIndex(idx oci.SignedImageIndex) (string, int64, error) {
	outfile := filepath.Join(bc.o.TempDir(), "index.json")

	b, err := idx.RawManifest()
	if err != nil {
		return "", 0, fmt.Errorf("getting raw manifest: %w", err)
	}
	if err := os.WriteFile(outfile, b, 0644); err != nil { //nolint:gosec // this file is fine to be readable
		return "", 0, fmt.Errorf("writing index file: %w", err)
	}

	stat, err := os.Stat(outfile)
	if err != nil {
		return "", 0, fmt.Errorf("stat(%q): %w", outfile, err)
	}

	bc.Logger().Infof("built index file as %s", outfile)
	return outfile, stat.Size(), nil
}

func (bc *Context) BuildPackageList(ctx context.Context) (toInstall []*repository.RepositoryPackage, conflicts []string, err error) {
	if toInstall, conflicts, err = bc.apk.ResolveWorld(ctx); err != nil {
		return toInstall, conflicts, fmt.Errorf("resolving apk packages: %w", err)
	}
	bc.Logger().Infof("finished gathering apk info in %s", bc.o.WorkDir)

	return toInstall, conflicts, err
}

func newSBOM(fsys apkfs.FullFS, o options.Options, ic types.ImageConfiguration, bde time.Time) *sbom.SBOM {
	s := sbom.NewWithFS(fsys, o.Arch)
	// Parse the image reference
	if len(o.Tags) > 0 {
		tag, err := name.NewTag(o.Tags[0])
		if err == nil {
			s.Options.ImageInfo.Tag = tag.TagStr()
			s.Options.ImageInfo.Name = tag.String()
		} else {
			o.Logger().Errorf("%s parsing tag %s, ignoring", o.Tags[0], err)
		}
	}

	s.Options.ImageInfo.SourceDateEpoch = bde
	s.Options.Formats = o.SBOMFormats
	s.Options.ImageInfo.VCSUrl = ic.VCSUrl

	if o.UseDockerMediaTypes {
		s.Options.ImageInfo.ImageMediaType = ggcrtypes.DockerManifestSchema2
	} else {
		s.Options.ImageInfo.ImageMediaType = ggcrtypes.OCIManifestSchema1
	}

	s.Options.OutputDir = o.TempDir()
	if o.SBOMPath != "" {
		s.Options.OutputDir = o.SBOMPath
	}

	return s
}

func (bc *Context) GenerateIndexSBOM(ctx context.Context, indexDigest name.Digest, imgs map[types.Architecture]oci.SignedImage) ([]types.SBOM, error) {
	_, span := otel.Tracer("apko").Start(ctx, "GenerateIndexSBOM")
	defer span.End()

	if !bc.WantSBOM() {
		bc.Logger().Warnf("skipping index SBOM generation")
		return nil, nil
	}

	s := newSBOM(bc.fs, bc.o, bc.ic, bc.o.SourceDateEpoch)
	bc.Logger().Infof("Generating index SBOM")

	// Add the image digest
	h, err := v1.NewHash(indexDigest.DigestStr())
	if err != nil {
		return nil, errors.New("getting index hash")
	}
	s.Options.ImageInfo.IndexDigest = h

	s.Options.ImageInfo.IndexMediaType = ggcrtypes.OCIImageIndex
	if bc.o.UseDockerMediaTypes {
		s.Options.ImageInfo.IndexMediaType = ggcrtypes.DockerManifestList
	}
	var ext string
	switch bc.o.SBOMFormats[0] {
	case "spdx":
		ext = "spdx.json"
	case "cyclonedx":
		ext = "cdx"
	case "idb":
		ext = "idb"
	}

	// Load the images data into the SBOM generator options
	archs := make([]types.Architecture, 0, len(imgs))
	for arch := range imgs {
		archs = append(archs, arch)
	}
	sort.Slice(archs, func(i, j int) bool {
		return archs[i].String() < archs[j].String()
	})
	for _, arch := range archs {
		i := imgs[arch]
		sbomHash, err := khash.SHA256ForFile(filepath.Join(s.Options.OutputDir, fmt.Sprintf("sbom-%s.%s", arch.ToAPK(), ext)))
		if err != nil {
			return nil, fmt.Errorf("checksumming %s SBOM: %w", arch, err)
		}

		d, err := i.Digest()
		if err != nil {
			return nil, fmt.Errorf("getting arch image digest: %w", err)
		}

		s.Options.ImageInfo.Images = append(
			s.Options.ImageInfo.Images,
			soptions.ArchImageInfo{
				Digest:     d,
				Arch:       arch,
				SBOMDigest: sbomHash,
			})
	}
	files, err := s.GenerateIndex()
	if err != nil {
		return nil, fmt.Errorf("generating index SBOM: %w", err)
	}
	var sboms = make([]types.SBOM, 0, len(files))
	for _, f := range files {
		sboms = append(sboms, types.SBOM{
			Path:   f,
			Format: bc.o.SBOMFormats[0],
			Digest: h,
		})
	}
	return sboms, nil
}
