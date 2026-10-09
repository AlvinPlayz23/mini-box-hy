package image

import (
	"compress/gzip"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Decompress wraps r with the right decompressor for a layer media type. The
// returned closer must be closed by the caller when non-nil.
func Decompress(r io.Reader, mediaType string) (io.Reader, io.Closer, error) {
	switch {
	case strings.Contains(mediaType, "gzip"), strings.HasSuffix(mediaType, ".tar.gzip"):
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip layer: %w", err)
		}
		return zr, zr, nil
	case strings.Contains(mediaType, "zstd"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("open zstd layer: %w", err)
		}
		return zr, zr.IOReadCloser(), nil
	case strings.Contains(mediaType, "tar"):
		return r, nil, nil
	default:
		// Unknown media types are assumed to be tar streams, which is what the
		// OCI spec allows for the uncompressed case.
		return r, nil, nil
	}
}

// DetectCompression sniffs a stream so that archives without a media type
// (for example a local tarball passed to `minibox load`) can still be handled.
func DetectCompression(head []byte) string {
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return MediaTypeLayerGzip
	case len(head) >= 4 && head[0] == 0x28 && head[1] == 0xb5 && head[2] == 0x2f && head[3] == 0xfd:
		return MediaTypeLayerZstd
	default:
		return MediaTypeLayerUncompressed
	}
}
