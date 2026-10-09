package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/AlvinPlayz23/minibox/internal/storage"
)

// PullOptions controls image pulling.
type PullOptions struct {
	// Platform selects the image variant; zero value means the host platform.
	Platform Platform
	// Concurrency is the number of layers downloaded at once (default 4).
	Concurrency int
	// Progress receives human-readable progress lines; may be nil.
	Progress func(string)
	// MaxUnpackBytes caps the total unpacked size of a layer (0 = unlimited).
	MaxUnpackBytes int64
}

// Pull resolves, downloads and unpacks an image into the store, returning the
// stored record. Layers already present are reused.
func Pull(ctx context.Context, store *Store, ref Reference, opts PullOptions) (*Record, error) {
	if err := store.EnsureDirs(); err != nil {
		return nil, err
	}
	if opts.Platform.OS == "" {
		opts.Platform = CurrentPlatform()
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	client := NewClient(ref.Registry)
	progress(fmt.Sprintf("resolving %s from %s", ref.Familiar(), client.Host()))
	manifest, manifestDigest, err := client.ResolveManifest(ctx, ref, opts.Platform)
	if err != nil {
		return nil, err
	}

	cfg, err := fetchConfig(ctx, store, client, ref, manifest)
	if err != nil {
		return nil, err
	}

	rec := NewRecord(ref, manifestDigest, manifest, cfg)
	chainDir := store.LayerDir(rec.ChainID)
	if store.HasLayerChain(rec.ChainID) {
		progress(fmt.Sprintf("layers for %s already unpacked", ref.Familiar()))
	} else {
		if err := downloadLayers(ctx, store, client, ref, manifest, opts, progress); err != nil {
			return nil, err
		}
		if err := unpackChain(store, ref, manifest, chainDir, opts, progress); err != nil {
			return nil, err
		}
	}

	if err := store.SaveRecord(rec); err != nil {
		return nil, err
	}
	progress(fmt.Sprintf("pulled %s (%d layers, %s)", ref.Familiar(), len(manifest.Layers), humanBytes(rec.Size)))
	return rec, nil
}

// fetchConfig downloads and parses the image config blob.
func fetchConfig(ctx context.Context, store *Store, client *Client, ref Reference, manifest *Manifest) (*ConfigFile, error) {
	path := store.BlobPath(manifest.Config.Digest)
	if !store.HasBlob(manifest.Config.Digest) {
		tmp, err := store.TempBlobPath()
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp)
		if _, err := client.DownloadBlob(ctx, ref.Repository, manifest.Config.Digest, tmp); err != nil {
			return nil, fmt.Errorf("download image config for %s: %w", ref.Familiar(), err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, fmt.Errorf("store image config: %w", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	var cfg ConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse image config for %s: %w", ref.Familiar(), err)
	}
	return &cfg, nil
}

// downloadLayers fetches every missing layer blob in parallel, verifying
// digests. Downloads never buffer a whole layer in memory: bytes stream from
// the registry straight to disk.
func downloadLayers(ctx context.Context, store *Store, client *Client, ref Reference, manifest *Manifest, opts PullOptions, progress func(string)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type job struct {
		index  int
		desc   Descriptor
		target string
		tmp    string
	}
	var jobs []job
	for i, l := range manifest.Layers {
		if store.HasBlob(l.Digest) {
			continue
		}
		tmp, err := store.TempBlobPath()
		if err != nil {
			return err
		}
		jobs = append(jobs, job{index: i, desc: l, target: store.BlobPath(l.Digest), tmp: tmp})
	}
	if len(jobs) == 0 {
		return nil
	}

	work := make(chan job)
	errs := make(chan error, len(jobs))
	var wg sync.WaitGroup
	workers := opts.Concurrency
	if workers > len(jobs) {
		workers = len(jobs)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				if _, err := client.DownloadBlob(ctx, ref.Repository, j.desc.Digest, j.tmp); err != nil {
					os.Remove(j.tmp)
					errs <- err
					cancel()
					return
				}
				if err := os.Rename(j.tmp, j.target); err != nil {
					errs <- fmt.Errorf("store blob %s: %w", j.desc.Digest, err)
					cancel()
					return
				}
				progress(fmt.Sprintf("downloaded layer %d/%d (%s)", j.index+1, len(manifest.Layers), humanBytes(j.desc.Size)))
			}
		}()
	}
	go func() {
		defer close(work)
		for _, j := range jobs {
			select {
			case work <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// unpackChain applies layers in order into the flattened chain directory. The
// directory is built under a temporary name and renamed into place so that a
// crash never leaves a half-unpacked chain behind.
func unpackChain(store *Store, ref Reference, manifest *Manifest, chainDir string, opts PullOptions, progress func(string)) error {
	parent := filepath.Dir(chainDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create layer directory %s: %w", parent, err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-unpack-")
	if err != nil {
		return fmt.Errorf("create temporary layer directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(tmp)
		}
	}()

	for i, layer := range manifest.Layers {
		if err := unpackOneLayer(store, ref, layer, tmp, opts); err != nil {
			return err
		}
		progress(fmt.Sprintf("unpacked layer %d/%d", i+1, len(manifest.Layers)))
	}
	if err := os.Rename(tmp, chainDir); err != nil {
		return fmt.Errorf("finalise layer chain: %w", err)
	}
	cleanup = false
	return nil
}

// unpackOneLayer streams a stored blob through its decompressor into the tar
// extractor.
func unpackOneLayer(store *Store, ref Reference, layer Descriptor, dest string, opts PullOptions) error {
	blobPath := store.BlobPath(layer.Digest)
	f, err := os.Open(blobPath)
	if err != nil {
		return fmt.Errorf("open layer blob %s: %w", layer.Digest, err)
	}
	defer f.Close()

	reader, closer, err := Decompress(f, layer.MediaType)
	if err != nil {
		return fmt.Errorf("layer %s of %s: %w", layer.Digest, ref.Familiar(), err)
	}
	if closer != nil {
		defer closer.Close()
	}
	stats, err := storage.UnpackTar(reader, storage.UnpackOptions{Dest: dest, MaxBytes: opts.MaxUnpackBytes})
	if err != nil {
		return fmt.Errorf("unpack layer %s of %s: %w", layer.Digest, ref.Familiar(), err)
	}
	_ = stats
	return nil
}

// humanBytes renders a byte count for progress output.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
