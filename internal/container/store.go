//go:build linux

// Package container implements container lifecycle: the on-disk store, state
// files with locking, the foreground supervisor and the detached shim.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RootEnv names the environment variable that overrides the state root.
const RootEnv = "MINIBOX_ROOT"

// DefaultRoot returns the state root: $MINIBOX_ROOT when set, otherwise
// /var/lib/minibox for root and ~/.local/share/minibox for everyone else.
func DefaultRoot() string {
	if root := os.Getenv(RootEnv); root != "" {
		return root
	}
	if os.Geteuid() == 0 {
		return "/var/lib/minibox"
	}
	if home := os.Getenv("XDG_DATA_HOME"); home != "" {
		return filepath.Join(home, "minibox")
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".local", "share", "minibox")
	}
	return filepath.Join(os.TempDir(), "minibox")
}

// Store is the on-disk container store rooted at Root.
type Store struct {
	Root string
}

// Open returns the store for the default root, creating its directories.
func Open() (*Store, error) { return OpenAt(DefaultRoot()) }

// OpenAt returns a store rooted at root, creating its directories.
func OpenAt(root string) (*Store, error) {
	s := &Store{Root: root}
	for _, dir := range []string{s.ContainersDir(), s.ImagesDir(), s.LayersDir(), s.BlobsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state directory %s: %w", dir, err)
		}
	}
	return s, nil
}

// ContainersDir is where per-container directories live.
func (s *Store) ContainersDir() string { return filepath.Join(s.Root, "containers") }

// ImagesDir is where image manifests and configs live.
func (s *Store) ImagesDir() string { return filepath.Join(s.Root, "images") }

// LayersDir is where unpacked layer diffs live.
func (s *Store) LayersDir() string { return filepath.Join(s.Root, "layers") }

// BlobsDir is where compressed layer blobs live.
func (s *Store) BlobsDir() string { return filepath.Join(s.Root, "blobs") }

// Dir returns a container's directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.ContainersDir(), id) }

// Exists reports whether a container directory exists.
func (s *Store) Exists(id string) bool {
	_, err := os.Stat(s.Dir(id))
	return err == nil
}

// NewID returns a fresh 64-hex-character container id.
func NewID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate container id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Resolve turns a (possibly abbreviated) id or name into a full container id.
func (s *Store) Resolve(prefix string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("empty container id")
	}
	ids, err := s.List()
	if err != nil {
		return "", err
	}
	var matches []string
	for _, id := range ids {
		if id == prefix {
			return id, nil
		}
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	// Names are stored in state.json; a full scan is cheap at this scale.
	named, err := s.findByName(prefix)
	if err != nil {
		return "", err
	}
	if named != "" {
		return named, nil
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no container matches %q: run `minibox ps -a` to list containers", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("container id %q is ambiguous (%d matches): use a longer prefix", prefix, len(matches))
	}
}

func (s *Store) findByName(name string) (string, error) {
	ids, err := s.List()
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		st, err := LoadState(filepath.Join(s.Dir(id), "state.json"))
		if err != nil {
			continue
		}
		if st.Name == name {
			return id, nil
		}
	}
	return "", nil
}

// List returns all container ids, newest first.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.ContainersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		id  string
		mod time.Time
	}
	var items []item
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{id: e.Name(), mod: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.id)
	}
	return ids, nil
}

// Remove deletes a container directory and everything in it.
func (s *Store) Remove(id string) error {
	if err := os.RemoveAll(s.Dir(id)); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// Prune removes container directories whose state marks them dead.
func (s *Store) Prune() ([]string, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range ids {
		st, err := LoadState(filepath.Join(s.Dir(id), "state.json"))
		if err != nil {
			continue
		}
		if st.Status == StatusExited || st.Status == StatusCreated {
			if !processAlive(st.PID) {
				if err := s.Remove(id); err == nil {
					removed = append(removed, id)
				}
			}
		}
	}
	return removed, nil
}

// Lock is an advisory exclusive lock over a file.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive flock on path, creating it when needed. It blocks
// until the lock is available.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// LockContainer locks a single container's lock file.
func (s *Store) LockContainer(id string) (*Lock, error) {
	if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
		return nil, err
	}
	return Acquire(filepath.Join(s.Dir(id), "lock"))
}

// LockStore locks the whole store (used by create, prune and garbage
// collection).
func (s *Store) LockStore() (*Lock, error) {
	return Acquire(filepath.Join(s.Root, "store.lock"))
}

// processAlive reports whether a pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return unix.Kill(pid, 0) == nil
}
