package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/transfer/registry"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"
	"github.com/cpuguy83/dockercfg"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var amd64Platform = ocispec.Platform{
	OS:           "linux",
	Architecture: "amd64",
}

const sourceDigestAnnotation = "io.github.cpuguy83.home-assistant-gzip-mirror.source.digest"

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, os.Args[0], "<source> <destination>")
	}
	flag.Parse()

	args := flag.Args()
	if len(args) != 2 {
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := mirror(ctx, args[0], args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)

		if errors.Is(err, context.Canceled) {
			sig := os.Interrupt.(syscall.Signal)
			os.Exit(128 + int(sig))
		}

		os.Exit(1)
	}
}

func mirror(ctx context.Context, source, destination string) error {
	dcfg, err := dockercfg.LoadDefaultConfig()
	if err != nil {
		return fmt.Errorf("loading Docker credentials: %w", err)
	}
	credentials := credFunc(dcfg.GetRegistryCredentials)

	destinationRef, err := reference.ParseNormalizedNamed(destination)
	if err != nil {
		return fmt.Errorf("parsing destination %q: %w", destination, err)
	}
	if _, ok := destinationRef.(reference.NamedTagged); !ok {
		return fmt.Errorf("destination %q must include a tag", destination)
	}
	destination = destinationRef.String()

	sourceRef, err := reference.ParseNormalizedNamed(source)
	if err != nil {
		return fmt.Errorf("parsing source %q: %w", source, err)
	}
	if _, ok := sourceRef.(reference.NamedTagged); !ok {
		return fmt.Errorf("source %q must include a tag", source)
	}
	source = sourceRef.String()

	srcReg, err := registry.NewOCIRegistry(ctx, source, registry.WithCredentials(credentials))
	if err != nil {
		return fmt.Errorf("creating source registry: %w", err)
	}

	dstReg, err := registry.NewOCIRegistry(ctx, destination, registry.WithCredentials(credentials))
	if err != nil {
		return fmt.Errorf("creating destination registry: %w", err)
	}

	_, srcDesc, err := srcReg.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolving source image: %w", err)
	}
	if !images.IsIndexType(srcDesc.MediaType) {
		return fmt.Errorf("source image is %q, not a manifest list or image index", srcDesc.MediaType)
	}

	current, err := destinationHasSourceDigest(ctx, dstReg, destination, srcDesc.Digest)
	if err != nil {
		return err
	}
	if current {
		fmt.Printf("%s already mirrors %s@%s\n", destination, source, srcDesc.Digest)
		return nil
	}

	fmt.Printf("Mirroring linux/amd64 from %s@%s\n", source, srcDesc.Digest)

	sourceFetcher, err := srcReg.Fetcher(ctx, source)
	if err != nil {
		return fmt.Errorf("creating source fetcher: %w", err)
	}
	sourceProvider := remoteProvider{fetcher: sourceFetcher}
	sourceManifestDescriptor, err := findPlatformManifest(ctx, sourceProvider, srcDesc, amd64Platform)
	if err != nil {
		return err
	}

	sourceManifest, err := fetchManifest(ctx, sourceProvider, sourceManifestDescriptor)
	if err != nil {
		return fmt.Errorf("reading source linux/amd64 manifest: %w", err)
	}

	sourceConfig, err := fetchConfig(ctx, sourceProvider, sourceManifest.Config)
	if err != nil {
		return fmt.Errorf("reading source configuration: %w", err)
	}
	if sourceConfig.RootFS.Type != "layers" || len(sourceConfig.RootFS.DiffIDs) != len(sourceManifest.Layers) {
		return fmt.Errorf("source configuration does not contain one diff ID for each layer")
	}

	config := sourceManifest.Config
	config.MediaType = ocispec.MediaTypeImageConfig
	configPusher, err := dstReg.Pusher(ctx, config)
	if err != nil {
		return fmt.Errorf("creating destination config pusher: %w", err)
	}
	if err := copyBlob(ctx, sourceFetcher, configPusher, sourceManifest.Config, config); err != nil {
		return fmt.Errorf("copying source configuration: %w", err)
	}

	layers := make([]ocispec.Descriptor, 0, len(sourceManifest.Layers))
	for i, sourceLayer := range sourceManifest.Layers {
		layer, err := mirrorLayer(ctx, sourceFetcher, dstReg, sourceLayer, sourceConfig.RootFS.DiffIDs[i])
		if err != nil {
			return fmt.Errorf("mirroring source layer %s: %w", sourceLayer.Digest, err)
		}
		layers = append(layers, layer)
	}

	destinationManifest := sourceManifest
	destinationManifest.MediaType = ocispec.MediaTypeImageManifest
	destinationManifest.Config = config
	destinationManifest.Layers = layers
	if destinationManifest.Annotations == nil {
		destinationManifest.Annotations = make(map[string]string)
	}
	destinationManifest.Annotations[sourceDigestAnnotation] = srcDesc.Digest.String()

	manifestBytes, err := json.Marshal(destinationManifest)
	if err != nil {
		return fmt.Errorf("encoding destination manifest: %w", err)
	}

	dstDesc := ocispec.Descriptor{
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
		MediaType: destinationManifest.MediaType,
	}

	manifestPusher, err := dstReg.Pusher(ctx, dstDesc)
	if err != nil {
		return fmt.Errorf("creating destination manifest pusher: %w", err)
	}

	if err := pushBlob(ctx, manifestPusher, dstDesc, bytes.NewReader(manifestBytes)); err != nil {
		return fmt.Errorf("publishing destination manifest: %w", err)
	}

	if err := verifyDestination(ctx, dstReg, destination); err != nil {
		return err
	}
	return nil
}

func destinationHasSourceDigest(ctx context.Context, destinationRegistry imageRegistry, destination string, sourceDigest digest.Digest) (bool, error) {
	_, descriptor, err := destinationRegistry.Resolve(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("resolving destination image: %w", err)
	}
	if descriptor.MediaType != ocispec.MediaTypeImageManifest {
		return false, nil
	}

	fetcher, err := destinationRegistry.Fetcher(ctx, destination)
	if err != nil {
		return false, fmt.Errorf("creating destination fetcher: %w", err)
	}
	manifest, err := fetchManifest(ctx, remoteProvider{fetcher: fetcher}, descriptor)
	if err != nil {
		return false, fmt.Errorf("reading destination manifest: %w", err)
	}
	return manifest.Annotations[sourceDigestAnnotation] == sourceDigest.String(), nil
}

func findPlatformManifest(ctx context.Context, provider content.Provider, root ocispec.Descriptor, platform ocispec.Platform) (ocispec.Descriptor, error) {
	var selected *ocispec.Descriptor
	children := images.ChildrenHandler(provider)
	handler := images.HandlerFunc(func(ctx context.Context, descriptor ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if images.IsManifestType(descriptor.MediaType) && descriptor.Platform != nil {
			if descriptor.Platform.OS == platform.OS && descriptor.Platform.Architecture == platform.Architecture {
				if selected != nil {
					return nil, fmt.Errorf("source image has multiple %s/%s manifests", platform.OS, platform.Architecture)
				}
				selected = &descriptor
			}
			return nil, images.ErrSkipDesc
		}
		return children.Handle(ctx, descriptor)
	})
	if err := images.Walk(ctx, handler, root); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("walking source manifest list: %w", err)
	}
	if selected == nil {
		return ocispec.Descriptor{}, fmt.Errorf("source image has no %s/%s manifest", platform.OS, platform.Architecture)
	}
	return *selected, nil
}

func fetchManifest(ctx context.Context, provider content.Provider, descriptor ocispec.Descriptor) (ocispec.Manifest, error) {
	contents, err := content.ReadBlob(ctx, provider, descriptor)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return ocispec.Manifest{}, err
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest &&
		manifest.MediaType != images.MediaTypeDockerSchema2Manifest {
		return ocispec.Manifest{}, fmt.Errorf("source manifest is %q, not an image manifest", manifest.MediaType)
	}
	return manifest, nil
}

func fetchConfig(ctx context.Context, provider content.Provider, descriptor ocispec.Descriptor) (ocispec.Image, error) {
	contents, err := content.ReadBlob(ctx, provider, descriptor)
	if err != nil {
		return ocispec.Image{}, err
	}
	var config ocispec.Image
	if err := json.Unmarshal(contents, &config); err != nil {
		return ocispec.Image{}, err
	}
	return config, nil
}

func mirrorLayer(ctx context.Context, fetcher registryFetcher, destinationRegistry *registry.OCIRegistry, source ocispec.Descriptor, diffID digest.Digest) (ocispec.Descriptor, error) {
	mediaType, decompress, err := destinationLayerMediaType(source.MediaType)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	destination := source
	if decompress {
		destination.MediaType = mediaType
		destination.Digest = diffID
		destination.Size = 0
		destination.Annotations = nil
	}

	pusher, err := destinationRegistry.Pusher(ctx, destination)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("creating destination layer pusher: %w", err)
	}
	if decompress {
		destination, err = streamUncompressedLayer(ctx, fetcher, pusher, source, destination)
		return destination, err
	}
	if err := copyBlob(ctx, fetcher, pusher, source, destination); err != nil {
		return ocispec.Descriptor{}, err
	}
	return destination, nil
}

func destinationLayerMediaType(mediaType string) (destination string, decompress bool, err error) {
	switch mediaType {
	case images.MediaTypeDockerSchema2LayerGzip,
		images.MediaTypeDockerSchema2LayerForeignGzip,
		images.MediaTypeDockerSchema2Layer,
		images.MediaTypeDockerSchema2LayerForeign,
		ocispec.MediaTypeImageLayerGzip,
		ocispec.MediaTypeImageLayerNonDistributableGzip,
		ocispec.MediaTypeImageLayer,
		ocispec.MediaTypeImageLayerNonDistributable:
		return mediaType, false, nil
	case ocispec.MediaTypeImageLayerZstd:
		return ocispec.MediaTypeImageLayer, true, nil
	case ocispec.MediaTypeImageLayerNonDistributableZstd:
		return ocispec.MediaTypeImageLayerNonDistributable, true, nil
	default:
		return "", false, fmt.Errorf("unsupported layer media type %q", mediaType)
	}
}

func streamUncompressedLayer(ctx context.Context, fetcher registryFetcher, pusher registryPusher, source, destination ocispec.Descriptor) (ocispec.Descriptor, error) {
	reader, err := fetcher.Fetch(ctx, source)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("fetching source layer for upload: %w", err)
	}
	defer reader.Close()

	writer, err := pusher.Push(ctx, destination)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			counter := &byteCounter{}
			if err := uncompressedStream(counter, reader); err != nil {
				return ocispec.Descriptor{}, err
			}
			destination.Size = counter.size
			return destination, nil
		}
		return ocispec.Descriptor{}, fmt.Errorf("opening destination layer upload: %w", err)
	}
	defer writer.Close()

	counter := &byteCounter{}
	if err := uncompressedStream(io.MultiWriter(writer, counter), reader); err != nil {
		return ocispec.Descriptor{}, err
	}
	destination.Size = counter.size
	if err := writer.Commit(ctx, destination.Size, destination.Digest); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("committing destination layer: %w", err)
	}
	return destination, nil
}

func uncompressedStream(dst io.Writer, src io.Reader) error {
	uncompressed, err := compression.DecompressStream(src)
	if err != nil {
		return fmt.Errorf("decompressing source layer: %w", err)
	}
	defer uncompressed.Close()

	if _, err := io.Copy(dst, uncompressed); err != nil {
		return fmt.Errorf("writing uncompressed source layer: %w", err)
	}
	return nil
}

func copyBlob(ctx context.Context, fetcher registryFetcher, pusher registryPusher, source, destination ocispec.Descriptor) error {
	reader, err := fetcher.Fetch(ctx, source)
	if err != nil {
		return fmt.Errorf("fetching source blob: %w", err)
	}
	defer reader.Close()
	return pushBlob(ctx, pusher, destination, reader)
}

func pushBlob(ctx context.Context, pusher registryPusher, descriptor ocispec.Descriptor, source io.Reader) error {
	writer, err := pusher.Push(ctx, descriptor)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("opening destination upload: %w", err)
	}
	defer writer.Close()
	if _, err := io.Copy(writer, source); err != nil {
		return fmt.Errorf("uploading content: %w", err)
	}
	if err := writer.Commit(ctx, descriptor.Size, descriptor.Digest); err != nil {
		return fmt.Errorf("committing content: %w", err)
	}
	return nil
}

func verifyDestination(ctx context.Context, dstReg *registry.OCIRegistry, destination string) error {
	_, descriptor, err := dstReg.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("resolving destination image: %w", err)
	}
	if !images.IsManifestType(descriptor.MediaType) {
		return fmt.Errorf("destination %q is %q, not an OCI-compatible image manifest", destination, descriptor.MediaType)
	}

	fetcher, err := dstReg.Fetcher(ctx, destination)
	if err != nil {
		return fmt.Errorf("creating destination fetcher: %w", err)
	}
	provider := remoteProvider{fetcher: fetcher}
	children := images.ChildrenHandler(provider)
	handler := images.HandlerFunc(func(ctx context.Context, current ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if current.Digest == descriptor.Digest {
			manifest, err := fetchManifest(ctx, provider, current)
			if err != nil {
				return nil, err
			}
			for _, layer := range manifest.Layers {
				switch layer.MediaType {
				case images.MediaTypeDockerSchema2Layer,
					images.MediaTypeDockerSchema2LayerGzip,
					images.MediaTypeDockerSchema2LayerForeign,
					images.MediaTypeDockerSchema2LayerForeignGzip,
					ocispec.MediaTypeImageLayer,
					ocispec.MediaTypeImageLayerGzip,
					ocispec.MediaTypeImageLayerNonDistributable,
					ocispec.MediaTypeImageLayerNonDistributableGzip:
				default:
					return nil, fmt.Errorf("destination contains unsupported layer %s with media type %q", layer.Digest, layer.MediaType)
				}
			}
		}
		return children.Handle(ctx, current)
	})
	if err := images.Walk(ctx, handler, descriptor); err != nil {
		return fmt.Errorf("walking destination manifest: %w", err)
	}
	return nil
}

type registryFetcher interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}

type registryPusher interface {
	Push(context.Context, ocispec.Descriptor) (content.Writer, error)
}

type imageRegistry interface {
	transfer.ImageResolver
	Fetcher(context.Context, string) (transfer.Fetcher, error)
}

type remoteProvider struct {
	fetcher registryFetcher
}

type readerToReaderAt struct {
	reader io.Reader
	ref    string
	pos    int64
	size   int64
}

func (r *readerToReaderAt) ReadAt(p []byte, off int64) (n int, retErr error) {
	defer func() {
		if retErr != nil && !errors.Is(retErr, io.EOF) {
			retErr = fmt.Errorf("error reading at offset %d for %s: %w", off, r.ref, retErr)
		}
	}()
	if ra, ok := r.reader.(io.ReaderAt); ok {
		n, err := ra.ReadAt(p, off)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, err
			}
			return n, fmt.Errorf("readerAt: %w", err)
		}
		return n, nil
	}

	if r.pos != off {
		if seeker, ok := r.reader.(io.Seeker); ok {
			// Seeking and then reading is not a faithful implementation of ReadAt,
			// but since we aren't planning to do parallel reads, this should be fine.
			if _, err := seeker.Seek(off, io.SeekStart); err != nil {
				return 0, fmt.Errorf("error seeking: %w", err)
			}
			r.pos = off
		} else {
			return 0, fmt.Errorf("reader does not support seeking: %s", r.ref)
		}
	}

	// Make sure we read at most the remaining bytes in the content OR at least the length of p.
	// This is part of the interface of [io.ReaderAt], which states that ReadAt should read len(p) bytes
	// or return a non-nil error.
	minRead := min(int(r.size-r.pos), len(p))
	n, err := io.ReadAtLeast(r.reader, p, int(minRead))
	if n > 0 {
		r.pos += int64(n)
	}
	return n, err
}

type contentReaderAt struct {
	desc ocispec.Descriptor
	io.ReaderAt
	io.Closer
}

func (cra *contentReaderAt) Size() int64 {
	return cra.desc.Size
}

func (provider remoteProvider) ReaderAt(ctx context.Context, descriptor ocispec.Descriptor) (content.ReaderAt, error) {
	reader, err := provider.fetcher.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}

	return &contentReaderAt{
		desc:     descriptor,
		ReaderAt: &readerToReaderAt{reader: reader, ref: descriptor.Digest.String(), size: descriptor.Size},
		Closer:   reader,
	}, nil
}

type byteCounter struct {
	size int64
}

func (counter *byteCounter) Write(data []byte) (int, error) {
	counter.size += int64(len(data))
	return len(data), nil
}

type credFunc func(host string) (string, string, error)

func (credentials credFunc) GetCredentials(_ context.Context, _, host string) (registry.Credentials, error) {
	host = dockercfg.ResolveRegistryHost(host)
	username, secret, err := credentials(host)
	if err != nil {
		return registry.Credentials{}, err
	}
	return registry.Credentials{
		Host:     host,
		Username: username,
		Secret:   secret,
	}, nil
}
