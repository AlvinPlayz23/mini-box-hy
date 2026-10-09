package image

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/storage"
)

// maxConfigSize bounds how much of a docker-save config file is read into
// memory.
const maxConfigSize = 8 << 20

// LoadOptions controls archive import.
type LoadOptions struct {
	// Tag names the imported image, for example "myapp:latest".
	Tag string
	// Progress receives human-readable progress lines; may be nil.
	Progress func(string)
	// MaxUnpackBytes caps the unpacked size of a layer (0 = unlimited).
	MaxUnpackBytes int64
}

// dockerSaveManifest is the manifest.json written by `docker save`.
type dockerSaveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// LoadArchive imports a local archive as an image. It accepts a plain rootfs
// tarball (optionally gzip or zstd compressed) or a `docker save` archive.
func LoadArchive(ctx context.Context, store *Store, path string, opts LoadOptions) (*Record, error) {
	if err := store.EnsureDirs(); err != nil {
		return nil, err
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	if opts.Tag == "" {
		opts.Tag = defaultTagFromPath(path)
	}
	ref, err := ParseReference(opts.Tag)
	if err != nil {
		return nil, fmt.Errorf("invalid tag %q: %w", opts.Tag, err)
	}

	manifest, err := readDockerSaveManifest(path)
	if err != nil {
		return nil, err
	}
	if manifest != nil {
		progress(fmt.Sprintf("importing docker save archive %s (%d layers)", path, len(manifest[0].Layers)))
		return loadDockerSave(ctx, store, path, manifest, ref, opts, progress)
	}
	progress(fmt.Sprintf("importing %s as a single-layer image", path))
	return loadRootfsTar(ctx, store, path, ref, opts, progress)
}

// defaultTagFromPath derives "name:latest" from an archive filename, dropping
// compound extensions such as .tar.gz.
func defaultTagFromPath(path string) string {
	base := filepath.Base(path)
	for _, ext := range []string{".tar.gz", ".tar.zst", ".tar.zstd", ".tar.xz", ".tgz", ".tar", ".gz", ".zst", ".zstd"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "imported"
	}
	return base + ":latest"
}

// readDockerSaveManifest scans an archive for manifest.json. It returns nil for
// archives that are not docker save output.
func readDockerSaveManifest(path string) ([]dockerSaveManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	reader, closer, err := decompressFile(f, path)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxConfigSize))
		if err != nil {
			return nil, fmt.Errorf("read manifest.json from %s: %w", path, err)
		}
		var manifests []dockerSaveManifest
		if err := json.Unmarshal(data, &manifests); err != nil {
			return nil, fmt.Errorf("parse manifest.json from %s: %w", path, err)
		}
		if len(manifests) == 0 {
			return nil, fmt.Errorf("manifest.json in %s lists no images", path)
		}
		return manifests, nil
	}
}

// loadRootfsTar imports a plain rootfs tarball as a single layer.
func loadRootfsTar(ctx context.Context, store *Store, path string, ref Reference, opts LoadOptions, progress func(string)) (*Record, error) {
	mediaType, err := sniffCompression(path)
	if err != nil {
		return nil, err
	}
	digest, size, err := storeBlobFromFile(ctx, store, path, mediaType)
	if err != nil {
		return nil, err
	}
	rec := &Record{
		Ref:        ref.Name(),
		Registry:   ref.Registry,
		Repository: ref.Repository,
		Tag:        ref.Tag,
		Digest:     digest,
		Layers:     []LayerRef{{Digest: digest, Size: size, MediaType: mediaType}},
		ChainID:    storage.ChainID([]string{digest}),
		Size:       size,
		Created:    time.Now().UTC(),
		Config: ConfigFile{
			Architecture: CurrentPlatform().Architecture,
			OS:           CurrentPlatform().OS,
		},
	}
	if err := unpackBlobToChain(store, rec, opts, progress); err != nil {
		return nil, err
	}
	if err := store.SaveRecord(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// loadDockerSave imports a `docker save` archive, preserving its layers.
func loadDockerSave(ctx context.Context, store *Store, path string, manifests []dockerSaveManifest, ref Reference, opts LoadOptions, progress func(string)) (*Record, error) {
	manifest := manifests[0]
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("archive %s has no layers", path)
	}

	cfg := ConfigFile{Architecture: CurrentPlatform().Architecture, OS: CurrentPlatform().OS}
	var cfgDigest string
	if manifest.Config != "" {
		data, digest, err := readArchiveEntry(path, manifest.Config, maxConfigSize)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse config %s from %s: %w", manifest.Config, path, err)
		}
		cfgDigest = digest
		if err := storeBlobBytes(store, cfgDigest, data); err != nil {
			return nil, err
		}
	}

	rec := &Record{
		Ref:          ref.Name(),
		Registry:     ref.Registry,
		Repository:   ref.Repository,
		Tag:          ref.Tag,
		ConfigDigest: cfgDigest,
		Created:      time.Now().UTC(),
		Config:       cfg,
	}
	var digests []string
	for _, layerPath := range manifest.Layers {
		mediaType, err := sniffCompression(path)
		if err != nil {
			return nil, err
		}
		digest, size, err := extractArchiveEntryToBlob(ctx, store, path, layerPath, mediaType)
		if err != nil {
			return nil, err
		}
		rec.Layers = append(rec.Layers, LayerRef{Digest: digest, Size: size, MediaType: mediaType})
		rec.Size += size
		digests = append(digests, digest)
	}
	rec.ChainID = storage.ChainID(digests)
	rec.Digest = rec.ChainID
	if err := unpackBlobToChain(store, rec, opts, progress); err != nil {
		return nil, err
	}
	if err := store.SaveRecord(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// unpackBlobToChain unpacks an image's layers into its flattened chain
// directory, unless that chain already exists.
func unpackBlobToChain(store *Store, rec *Record, opts LoadOptions, progress func(string)) error {
	chainDir := store.LayerDir(rec.ChainID)
	if store.HasLayerChain(rec.ChainID) {
		progress("layers already unpacked")
		return nil
	}
	parent := filepath.Dir(chainDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-unpack-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmp)
		}
	}()
	for i, layer := range rec.Layers {
		f, err := os.Open(store.BlobPath(layer.Digest))
		if err != nil {
			return fmt.Errorf("open layer blob: %w", err)
		}
		reader, closer, err := Decompress(f, layer.MediaType)
		if err != nil {
			f.Close()
			return err
		}
		_, err = storage.UnpackTar(reader, storage.UnpackOptions{Dest: tmp, MaxBytes: opts.MaxUnpackBytes})
		if closer != nil {
			closer.Close()
		}
		f.Close()
		if err != nil {
			return fmt.Errorf("unpack layer %d: %w", i+1, err)
		}
		progress(fmt.Sprintf("unpacked layer %d/%d", i+1, len(rec.Layers)))
	}
	if err := os.Rename(tmp, chainDir); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// sniffCompression reports the compression of a file from its magic bytes.
func sniffCompression(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return DetectCompression(head[:n]), nil
}

// decompressFile opens a file and wraps it in the right decompressor based on
// its magic bytes.
func decompressFile(f *os.File, path string) (io.Reader, io.Closer, error) {
	head := make([]byte, 4)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	return Decompress(f, DetectCompression(head[:n]))
}

// storeBlobFromFile copies a file into the blob store, returning its digest.
func storeBlobFromFile(ctx context.Context, store *Store, path, mediaType string) (string, int64, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer src.Close()

	h := sha256.New()
	tmp, err := store.TempBlobPath()
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp)
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	size, err := io.Copy(io.MultiWriter(dst, h), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, fmt.Errorf("copy %s into the blob store: %w", path, err)
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp, store.BlobPath(digest)); err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

// storeBlobBytes writes an in-memory blob.
func storeBlobBytes(store *Store, digest string, data []byte) error {
	if digest == "" {
		return nil
	}
	if store.HasBlob(digest) {
		return nil
	}
	tmp, err := store.TempBlobPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, store.BlobPath(digest))
}

// readArchiveEntry returns one entry of an archive, read into memory.
func readArchiveEntry(path, name string, limit int64) ([]byte, string, error) {
	if name == "" {
		return nil, "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	reader, closer, err := decompressFile(f, path)
	if err != nil {
		return nil, "", err
	}
	if closer != nil {
		defer closer.Close()
	}
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, "", fmt.Errorf("entry %s not found in %s", name, path)
		}
		if err != nil {
			return nil, "", err
		}
		if hdr.Name != name {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, limit))
		if err != nil {
			return nil, "", err
		}
		sum := sha256.Sum256(data)
		return data, "sha256:" + hex.EncodeToString(sum[:]), nil
	}
}

// extractArchiveEntryToBlob streams one entry of an archive into the blob
// store, hashing it on the way.
func extractArchiveEntryToBlob(ctx context.Context, store *Store, path, name, mediaType string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	reader, closer, err := decompressFile(f, path)
	if err != nil {
		return "", 0, err
	}
	if closer != nil {
		defer closer.Close()
	}
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", 0, fmt.Errorf("layer %s not found in %s", name, path)
		}
		if err != nil {
			return "", 0, err
		}
		if hdr.Name != name {
			continue
		}
		h := sha256.New()
		tmp, err := store.TempBlobPath()
		if err != nil {
			return "", 0, err
		}
		defer os.Remove(tmp)
		dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", 0, err
		}
		size, err := io.Copy(io.MultiWriter(dst, h), tr)
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", 0, fmt.Errorf("extract %s: %w", name, err)
		}
		digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
		if err := os.Rename(tmp, store.BlobPath(digest)); err != nil {
			return "", 0, err
		}
		return digest, size, nil
	}
}
