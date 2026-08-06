package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestUncompressedStream(t *testing.T) {
	t.Run("when a zstd layer is converted", func(t *testing.T) {
		const contents = "layer contents"
		var source bytes.Buffer
		writer, err := compression.CompressStream(&source, compression.Zstd)
		if err != nil {
			t.Fatalf("creating zstd layer: %v", err)
		}
		if _, err := io.WriteString(writer, contents); err != nil {
			t.Fatalf("writing zstd layer: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("closing zstd layer: %v", err)
		}
		sourceContents := append([]byte(nil), source.Bytes()...)

		var destination bytes.Buffer
		if err := uncompressedStream(&destination, &source); err != nil {
			t.Fatalf("converting source layer: %v", err)
		}
		var secondDestination bytes.Buffer
		if err := uncompressedStream(&secondDestination, bytes.NewReader(sourceContents)); err != nil {
			t.Fatalf("converting source layer a second time: %v", err)
		}
		if got, want := digest.FromBytes(secondDestination.Bytes()), digest.FromBytes(destination.Bytes()); got != want {
			t.Errorf("second uncompressed digest = %s, want %s", got, want)
		}
		reader, err := compression.DecompressStream(&destination)
		if err != nil {
			t.Fatalf("decompressing destination layer: %v", err)
		}
		defer reader.Close()
		if gotCompression := reader.GetCompression(); gotCompression != compression.Uncompressed {
			t.Errorf("destination compression = %v, want uncompressed", gotCompression)
		}
		destinationContents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading destination layer: %v", err)
		}
		if got := string(destinationContents); got != contents {
			t.Errorf("destination contents = %q, want %q", got, contents)
		}
	})
}

func TestFindPlatformManifest(t *testing.T) {
	t.Run("when an index contains several platforms", func(t *testing.T) {
		amd64 := ocispec.Descriptor{
			Digest:    digest.FromString("linux-amd64"),
			MediaType: images.MediaTypeDockerSchema2Manifest,
			Platform:  &amd64Platform,
		}
		index := ocispec.Index{
			MediaType: images.MediaTypeDockerSchema2ManifestList,
			Manifests: []ocispec.Descriptor{
				amd64,
				{
					Digest:    digest.FromString("linux-arm64"),
					MediaType: images.MediaTypeDockerSchema2Manifest,
					Platform: &ocispec.Platform{
						OS:           "linux",
						Architecture: "arm64",
					},
				},
			},
		}
		indexBytes, err := json.Marshal(index)
		if err != nil {
			t.Fatalf("marshalling index: %v", err)
		}
		indexDescriptor := ocispec.Descriptor{
			Digest:    digest.FromBytes(indexBytes),
			Size:      int64(len(indexBytes)),
			MediaType: images.MediaTypeDockerSchema2ManifestList,
		}
		fetcher := testFetcher{contents: map[digest.Digest][]byte{
			indexDescriptor.Digest: indexBytes,
		}}

		selected, err := findPlatformManifest(context.Background(), remoteProvider{fetcher: fetcher}, indexDescriptor, amd64Platform)
		if err != nil {
			t.Fatalf("finding amd64 manifest: %v", err)
		}
		if selected.Digest != amd64.Digest {
			t.Errorf("selected manifest = %s, want %s", selected.Digest, amd64.Digest)
		}
	})
}

func TestDestinationLayerMediaType(t *testing.T) {
	testCases := []struct {
		name        string
		source      string
		destination string
		decompress  bool
	}{
		{
			name:        "an OCI gzip layer stays gzip",
			source:      ocispec.MediaTypeImageLayerGzip,
			destination: ocispec.MediaTypeImageLayerGzip,
		},
		{
			name:        "a Docker gzip layer stays gzip",
			source:      images.MediaTypeDockerSchema2LayerGzip,
			destination: images.MediaTypeDockerSchema2LayerGzip,
		},
		{
			name:        "an OCI uncompressed layer stays uncompressed",
			source:      ocispec.MediaTypeImageLayer,
			destination: ocispec.MediaTypeImageLayer,
		},
		{
			name:        "a Docker uncompressed layer stays uncompressed",
			source:      images.MediaTypeDockerSchema2Layer,
			destination: images.MediaTypeDockerSchema2Layer,
		},
		{
			name:        "an OCI zstd layer is decompressed",
			source:      ocispec.MediaTypeImageLayerZstd,
			destination: ocispec.MediaTypeImageLayer,
			decompress:  true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			destination, decompress, err := destinationLayerMediaType(testCase.source)
			if err != nil {
				t.Fatalf("converting media type: %v", err)
			}
			if destination != testCase.destination {
				t.Errorf("destination media type = %q, want %q", destination, testCase.destination)
			}
			if decompress != testCase.decompress {
				t.Errorf("decompress = %v, want %v", decompress, testCase.decompress)
			}
		})
	}
}

func TestDestinationHasSourceDigest(t *testing.T) {
	const destination = "ghcr.io/cpuguy83/home-assistant-gzip-mirror:test"
	sourceDigest := digest.FromString("source index")

	t.Run("when the destination records the resolved source index digest", func(t *testing.T) {
		destinationRegistry := registryWithManifest(t, ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Annotations: map[string]string{
				sourceDigestAnnotation: sourceDigest.String(),
			},
		})

		current, err := destinationHasSourceDigest(context.Background(), destinationRegistry, destination, sourceDigest)
		if err != nil {
			t.Fatalf("checking destination source digest: %v", err)
		}
		if !current {
			t.Error("destination was not considered current")
		}
	})

	t.Run("when the destination records a different source index digest", func(t *testing.T) {
		destinationRegistry := registryWithManifest(t, ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Annotations: map[string]string{
				sourceDigestAnnotation: digest.FromString("older source index").String(),
			},
		})

		current, err := destinationHasSourceDigest(context.Background(), destinationRegistry, destination, sourceDigest)
		if err != nil {
			t.Fatalf("checking destination source digest: %v", err)
		}
		if current {
			t.Error("destination was considered current")
		}
	})

	t.Run("when the destination does not exist", func(t *testing.T) {
		destinationRegistry := testImageRegistry{resolveErr: errdefs.ErrNotFound}

		current, err := destinationHasSourceDigest(context.Background(), destinationRegistry, destination, sourceDigest)
		if err != nil {
			t.Fatalf("checking destination source digest: %v", err)
		}
		if current {
			t.Error("missing destination was considered current")
		}
	})

	t.Run("when a Docker manifest contains the annotation", func(t *testing.T) {
		destinationRegistry := registryWithManifest(t, ocispec.Manifest{
			MediaType: images.MediaTypeDockerSchema2Manifest,
			Annotations: map[string]string{
				sourceDigestAnnotation: sourceDigest.String(),
			},
		})

		current, err := destinationHasSourceDigest(context.Background(), destinationRegistry, destination, sourceDigest)
		if err != nil {
			t.Fatalf("checking destination source digest: %v", err)
		}
		if current {
			t.Error("non-OCI destination annotation was trusted")
		}
	})
}

func TestRemoteProvider(t *testing.T) {
	t.Run("when a remote reader is opened", func(t *testing.T) {
		contents := []byte("manifest contents")
		reader := &trackingReadCloser{reader: bytes.NewReader(contents)}
		provider := remoteProvider{fetcher: registryFetcherFunc(func(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
			return reader, nil
		})}

		readerAt, err := provider.ReaderAt(context.Background(), ocispec.Descriptor{
			Digest: digest.FromBytes(contents),
			Size:   int64(len(contents)),
		})
		if err != nil {
			t.Fatalf("opening remote reader: %v", err)
		}
		if reader.bytesRead != 0 {
			t.Fatalf("bytes read while opening = %d, want 0", reader.bytesRead)
		}

		buffer := make([]byte, 8)
		if _, err := readerAt.ReadAt(buffer, 0); err != nil {
			t.Fatalf("reading remote content: %v", err)
		}
		if reader.bytesRead != len(buffer) {
			t.Errorf("bytes read = %d, want %d", reader.bytesRead, len(buffer))
		}
		if err := readerAt.Close(); err != nil {
			t.Fatalf("closing remote reader: %v", err)
		}
		if !reader.closed {
			t.Error("remote reader was not closed")
		}
	})
}

func registryWithManifest(t *testing.T, manifest ocispec.Manifest) testImageRegistry {
	t.Helper()

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshalling manifest: %v", err)
	}
	descriptor := ocispec.Descriptor{
		Digest:    digest.FromBytes(manifestBytes),
		Size:      int64(len(manifestBytes)),
		MediaType: manifest.MediaType,
	}
	return testImageRegistry{
		descriptor: descriptor,
		fetcher: testFetcher{contents: map[digest.Digest][]byte{
			descriptor.Digest: manifestBytes,
		}},
	}
}

type testImageRegistry struct {
	descriptor ocispec.Descriptor
	resolveErr error
	fetcher    transfer.Fetcher
	fetcherErr error
}

func (registry testImageRegistry) Resolve(context.Context) (string, ocispec.Descriptor, error) {
	return "", registry.descriptor, registry.resolveErr
}

func (registry testImageRegistry) Fetcher(context.Context, string) (transfer.Fetcher, error) {
	return registry.fetcher, registry.fetcherErr
}

type testFetcher struct {
	contents map[digest.Digest][]byte
}

func (fetcher testFetcher) Fetch(_ context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	contents, ok := fetcher.contents[descriptor.Digest]
	if !ok {
		return nil, fmt.Errorf("content %s not found", descriptor.Digest)
	}
	return io.NopCloser(bytes.NewReader(contents)), nil
}

type registryFetcherFunc func(context.Context, ocispec.Descriptor) (io.ReadCloser, error)

func (fetcher registryFetcherFunc) Fetch(ctx context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	return fetcher(ctx, descriptor)
}

type trackingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (reader *trackingReadCloser) Read(data []byte) (int, error) {
	n, err := reader.reader.Read(data)
	reader.bytesRead += n
	return n, err
}

func (reader *trackingReadCloser) Close() error {
	reader.closed = true
	return nil
}
