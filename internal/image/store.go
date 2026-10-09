package image

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/storage"
)

// Store is the on-disk image store: blobs, flattened layer chains and image
// records.
type Store struct {
	root *container.Store
}

// OpenStore returns the image store of the given container store.
func OpenStore(root *container.Store) *Store { return &Store{root: root} }

// Root returns the container store backing this image store.
func (s *Store) Root() *container.Store { return s.root }

// BlobPath returns the path of a compressed blob.
func (s *Store) BlobPath(digest string) string {
	return filepath.Join(s.root.BlobsDir(), digestPath(digest))
}

// LayerDir returns the flattened directory for a layer chain.
func (s *Store) LayerDir(chainID string) string {
	return filepath.Join(s.root.LayersDir(), chainID, "diff")
}

// RecordPath returns the path of the record file for a reference.
func (s *Store) RecordPath(ref Reference) string {
	name := ref.Identifier()
	if ref.IsDigest() {
		// Colons are legal on Linux but awkward in shell globs; normalise.
		name = strings.ReplaceAll(name, ":", "-")
	}
	return filepath.Join(s.root.ImagesDir(), ref.Registry, ref.Repository, name+".json")
}

// HasBlob reports whether a blob is already stored.
func (s *Store) HasBlob(digest string) bool {
	_, err := os.Stat(s.BlobPath(digest))
	return err == nil
}

// EnsureDirs creates the store's subdirectories. Callers that write blobs,
// layers or records use it before their first write.
func (s *Store) EnsureDirs() error {
	for _, dir := range []string{
		filepath.Join(s.root.BlobsDir(), "sha256"),
		s.root.LayersDir(),
		s.root.ImagesDir(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create image store directory %s: %w", dir, err)
		}
	}
	return nil
}

// HasLayerChain reports whether a layer chain has been unpacked.
func (s *Store) HasLayerChain(chainID string) bool {
	info, err := os.Stat(s.LayerDir(chainID))
	return err == nil && info.IsDir()
}

// SaveRecord writes an image record, creating its directory.
func (s *Store) SaveRecord(rec *Record) error {
	ref, err := ParseReference(rec.Ref)
	if err != nil {
		return err
	}
	path := s.RecordPath(ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create image directory: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write image record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write image record: %w", err)
	}
	return nil
}

// LoadRecord reads an image record by reference.
func (s *Store) LoadRecord(ref Reference) (*Record, error) {
	path := s.RecordPath(ref)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("image %s is not in the local store: run `minibox pull %s` first", ref.Familiar(), ref.Familiar())
		}
		return nil, fmt.Errorf("read image record %s: %w", path, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse image record %s: %w", path, err)
	}
	return &rec, nil
}

// ResolveRecord loads an image by tag, falling back to the single record in the
// repository when only a repository name was given.
func (s *Store) ResolveRecord(name string) (*Record, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return nil, err
	}
	rec, err := s.LoadRecord(ref)
	if err == nil {
		return rec, nil
	}
	// A bare "alpine" should find alpine:latest, and any tag should find a
	// digest-pinned record for the same repository.
	dir := filepath.Dir(s.RecordPath(ref))
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		return nil, err
	}
	if len(entries) == 1 {
		return s.LoadRecord(Reference{Registry: ref.Registry, Repository: ref.Repository, Tag: strings.TrimSuffix(entries[0].Name(), ".json")})
	}
	return nil, err
}

// ListRecords returns every stored image record.
func (s *Store) ListRecords() ([]*Record, error) {
	var out []*Record
	root := s.root.ImagesDir()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// RemoveRecord deletes an image record and any layer chain no other image uses.
// It returns the removed layer chain id, if any.
func (s *Store) RemoveRecord(name string) (chainID string, err error) {
	rec, err := s.ResolveRecord(name)
	if err != nil {
		return "", err
	}
	ref, err := ParseReference(rec.Ref)
	if err != nil {
		return "", err
	}
	if err := os.Remove(s.RecordPath(ref)); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove image record: %w", err)
	}
	// Prune empty repository directories.
	_ = os.Remove(filepath.Dir(s.RecordPath(ref)))

	records, err := s.ListRecords()
	if err != nil {
		return "", err
	}
	for _, other := range records {
		if other.ChainID == rec.ChainID {
			return "", nil
		}
	}
	if err := os.RemoveAll(filepath.Join(s.root.LayersDir(), rec.ChainID)); err != nil {
		return "", fmt.Errorf("remove layer chain %s: %w", rec.ChainID, err)
	}
	return rec.ChainID, nil
}

// PruneBlobs removes blobs that no stored image references. It returns the
// digests it deleted.
func (s *Store) PruneBlobs() ([]string, error) {
	records, err := s.ListRecords()
	if err != nil {
		return nil, err
	}
	referenced := map[string]bool{}
	for _, rec := range records {
		for _, l := range rec.Layers {
			referenced[l.Digest] = true
		}
		referenced[rec.ConfigDigest] = true
		referenced[rec.Digest] = true
	}
	var removed []string
	dir := filepath.Join(s.root.BlobsDir(), "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, e := range entries {
		digest := "sha256:" + e.Name()
		if referenced[digest] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed = append(removed, digest)
		}
	}
	// Remove layer chains with no record, and stale temp files.
	records, _ = s.ListRecords()
	live := map[string]bool{}
	for _, rec := range records {
		live[rec.ChainID] = true
	}
	if chains, err := os.ReadDir(s.root.LayersDir()); err == nil {
		for _, e := range chains {
			if live[e.Name()] {
				continue
			}
			_ = os.RemoveAll(filepath.Join(s.root.LayersDir(), e.Name()))
		}
	}
	if tmp, err := os.ReadDir(s.root.BlobsDir()); err == nil {
		for _, e := range tmp {
			if strings.HasPrefix(e.Name(), "tmp-") {
				_ = os.RemoveAll(filepath.Join(s.root.BlobsDir(), e.Name()))
			}
		}
	}
	return removed, nil
}

// DiskUsage reports the number of bytes used by blobs and layer chains.
func (s *Store) DiskUsage() (blobs, layers int64, err error) {
	blobs, err = dirSize(filepath.Join(s.root.BlobsDir()))
	if err != nil {
		return 0, 0, err
	}
	layers, err = dirSize(s.root.LayersDir())
	return blobs, layers, err
}

// TempBlobPath returns a unique temporary path for an in-progress download.
func (s *Store) TempBlobPath() (string, error) {
	dir := s.root.BlobsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	return name, nil
}

// NewRecord builds a record from a resolved manifest and config.
func NewRecord(ref Reference, manifestDigest string, manifest *Manifest, cfg *ConfigFile) *Record {
	rec := &Record{
		Ref:          ref.Name(),
		Registry:     ref.Registry,
		Repository:   ref.Repository,
		Tag:          ref.Tag,
		Digest:       manifestDigest,
		ConfigDigest: manifest.Config.Digest,
		Created:      time.Now().UTC(),
		Config:       *cfg,
	}
	var digests []string
	for _, l := range manifest.Layers {
		rec.Layers = append(rec.Layers, LayerRef{Digest: l.Digest, Size: l.Size, MediaType: l.MediaType})
		rec.Size += l.Size
		digests = append(digests, l.Digest)
	}
	rec.ChainID = storage.ChainID(digests)
	return rec
}

// digestPath maps "sha256:abc" to "sha256/abc".
func digestPath(digest string) string {
	return strings.Replace(digest, ":", "/", 1)
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}
