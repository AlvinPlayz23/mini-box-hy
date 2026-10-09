//go:build linux

// Package volume manages named volumes: directories under <root>/volumes that
// containers can mount by name.
package volume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrInUse is returned when a volume is still referenced by a container.
var ErrInUse = errors.New("volume is in use")

// Volume is a named volume.
type Volume struct {
	Name       string            `json:"name"`
	Created    time.Time         `json:"created"`
	Mountpoint string            `json:"mountpoint"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// Store owns the volumes directory.
type Store struct {
	root string
}

// Open returns the volume store rooted at root (normally
// <MINIBOX_ROOT>/volumes).
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create volume directory %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Root returns the volumes directory.
func (s *Store) Root() string { return s.root }

// nameRe matches volume names: letters, digits, underscore, dot and dash, not
// starting with a dash.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ValidateName checks a volume name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid volume name %q: use letters, digits, underscore, dot or dash", name)
	}
	return nil
}

// IsName reports whether a -v source should be treated as a named volume rather
// than a host path.
func IsName(source string) bool {
	if source == "" {
		return false
	}
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~") {
		return false
	}
	return nameRe.MatchString(source)
}

// dir returns the directory of a volume.
func (s *Store) dir(name string) string { return filepath.Join(s.root, name) }

// DataDir returns the directory mounted into containers.
func (s *Store) DataDir(name string) string { return filepath.Join(s.dir(name), "_data") }

// metaPath returns the metadata file of a volume.
func (s *Store) metaPath(name string) string { return filepath.Join(s.dir(name), "volume.json") }

// Create makes a new volume. Creating an existing volume is not an error; the
// existing volume is returned.
func (s *Store) Create(name string) (*Volume, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if v, err := s.Get(name); err == nil {
		return v, nil
	}
	data := s.DataDir(name)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, fmt.Errorf("create volume %s: %w", name, err)
	}
	v := &Volume{
		Name:       name,
		Created:    time.Now().UTC(),
		Mountpoint: data,
	}
	if err := s.save(v); err != nil {
		return nil, err
	}
	return v, nil
}

// Get reads a volume's metadata.
func (s *Store) Get(name string) (*Volume, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.metaPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no such volume %q: run `minibox volume create %s` first", name, name)
		}
		return nil, fmt.Errorf("read volume %s: %w", name, err)
	}
	var v Volume
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse volume %s: %w", name, err)
	}
	if v.Mountpoint == "" {
		v.Mountpoint = s.DataDir(name)
	}
	return &v, nil
}

// Ensure returns an existing volume or creates it, which is what `run -v`
// needs.
func (s *Store) Ensure(name string) (*Volume, error) {
	if v, err := s.Get(name); err == nil {
		return v, nil
	}
	return s.Create(name)
}

// List returns all volumes, sorted by name.
func (s *Store) List() ([]*Volume, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Volume
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v, err := s.Get(e.Name())
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes a volume. A volume that is still referenced by a container is
// refused unless force is set.
func (s *Store) Remove(name string, force bool, inUse map[string]bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := s.Get(name); err != nil {
		return err
	}
	if inUse[name] && !force {
		return fmt.Errorf("%w: %q is mounted by a container; stop and remove it first or use -f", ErrInUse, name)
	}
	if err := os.RemoveAll(s.dir(name)); err != nil {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

// Prune removes volumes that no container references.
func (s *Store) Prune(inUse map[string]bool) ([]string, error) {
	volumes, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, v := range volumes {
		if inUse[v.Name] {
			continue
		}
		if err := s.Remove(v.Name, false, nil); err == nil {
			removed = append(removed, v.Name)
		}
	}
	return removed, nil
}

func (s *Store) save(v *Volume) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.metaPath(v.Name) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write volume %s: %w", v.Name, err)
	}
	return os.Rename(tmp, s.metaPath(v.Name))
}

// NamesInUse collects the volume names referenced by container mount specs of
// the form SOURCE:DEST[:ro].
func NamesInUse(mounts [][]string) map[string]bool {
	inUse := map[string]bool{}
	for _, list := range mounts {
		for _, spec := range list {
			source, _, _ := strings.Cut(spec, ":")
			if IsName(source) {
				inUse[source] = true
			}
		}
	}
	return inUse
}
