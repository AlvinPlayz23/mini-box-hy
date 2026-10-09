//go:build linux

// Package storage unpacks image layers into the content-addressed layer store
// and mounts them with overlayfs for containers.
package storage

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Whiteout markers used by the OCI layer format.
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// UnpackOptions controls layer extraction.
type UnpackOptions struct {
	// Dest is the directory the layer is unpacked into. It is created when
	// missing.
	Dest string
	// MaxBytes aborts extraction once this many bytes have been written
	// (0 means no limit). It protects against decompression bombs.
	MaxBytes int64
	// SkipChown avoids chown calls entirely (useful for tests and for
	// unprivileged stores that already own the files).
	SkipChown bool
}

// UnpackStats summarises an extraction.
type UnpackStats struct {
	Files     int   `json:"files"`
	Dirs      int   `json:"dirs"`
	Symlinks  int   `json:"symlinks"`
	Hardlinks int   `json:"hardlinks"`
	Devices   int   `json:"devices"`
	Whiteouts int   `json:"whiteouts"`
	Bytes     int64 `json:"bytes"`
}

// ChainID computes the content-addressed id of an ordered list of layer
// digests, following the same idea as containerd's chain ids.
func ChainID(digests []string) string {
	h := sha256.New()
	for _, d := range digests {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// UnpackTar extracts a layer tar stream into opts.Dest, applying OCI whiteouts
// and refusing any entry that would escape the destination directory.
func UnpackTar(r io.Reader, opts UnpackOptions) (UnpackStats, error) {
	var stats UnpackStats
	dest := opts.Dest
	if dest == "" {
		return stats, errors.New("unpack: empty destination")
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return stats, fmt.Errorf("unpack: resolve destination: %w", err)
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return stats, fmt.Errorf("unpack: create destination: %w", err)
	}

	// created tracks paths written by this layer so that an opaque-directory
	// marker does not delete files this layer just added.
	created := make(map[string]bool)
	type dirMeta struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirMeta

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("read layer tar: %w", err)
		}
		name, err := cleanEntryPath(hdr.Name)
		if err != nil {
			return stats, err
		}
		if name == "" {
			continue
		}
		base := path.Base(name)

		switch {
		case base == whiteoutOpaque:
			dir := path.Dir(name)
			if dir == "." {
				dir = ""
			}
			if err := clearDir(absDest, dir, created); err != nil {
				return stats, fmt.Errorf("apply opaque marker %q: %w", hdr.Name, err)
			}
			stats.Whiteouts++
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			target := path.Join(path.Dir(name), strings.TrimPrefix(base, whiteoutPrefix))
			if target == "." || target == "" {
				return stats, fmt.Errorf("invalid whiteout entry %q", hdr.Name)
			}
			if err := removeEntry(absDest, target); err != nil {
				return stats, fmt.Errorf("apply whiteout %q: %w", hdr.Name, err)
			}
			stats.Whiteouts++
			continue
		}

		target, err := secureJoin(absDest, name)
		if err != nil {
			return stats, err
		}
		if hdr.Typeflag != tar.TypeDir {
			// Archives are not required to carry explicit directory entries.
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return stats, fmt.Errorf("create parent directory for %s: %w", name, err)
			}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := makeDir(target, hdr); err != nil {
				return stats, err
			}
			dirs = append(dirs, dirMeta{path: target, hdr: hdr})
			created[name] = true
			stats.Dirs++

		case tar.TypeReg:
			n, err := writeRegularFile(target, hdr, tr, opts)
			if err != nil {
				return stats, err
			}
			stats.Bytes += n
			stats.Files++
			created[name] = true

		case tar.TypeSymlink:
			if err := replaceSymlink(target, hdr.Linkname); err != nil {
				return stats, err
			}
			stats.Symlinks++
			created[name] = true

		case tar.TypeLink:
			linkName, err := cleanEntryPath(hdr.Linkname)
			if err != nil || linkName == "" {
				return stats, fmt.Errorf("invalid hardlink target %q in entry %q", hdr.Linkname, hdr.Name)
			}
			linkTarget, err := secureJoin(absDest, linkName)
			if err != nil {
				return stats, fmt.Errorf("hardlink %q -> %q: %w", hdr.Name, hdr.Linkname, err)
			}
			if err := replaceWithHardlink(target, linkTarget); err != nil {
				return stats, err
			}
			stats.Hardlinks++
			created[name] = true

		case tar.TypeChar, tar.TypeBlock:
			if err := makeDevice(target, hdr); err != nil {
				return stats, err
			}
			stats.Devices++
			created[name] = true

		case tar.TypeFifo:
			if err := replaceFifo(target, hdr); err != nil {
				return stats, err
			}
			stats.Devices++
			created[name] = true

		default:
			// Global headers and unknown types carry no filesystem content.
			continue
		}

		if !opts.SkipChown {
			chown(target, hdr)
		}
	}

	// Directory metadata is applied last so that writes into a directory do not
	// disturb its mode, ownership or mtime.
	for i := len(dirs) - 1; i >= 0; i-- {
		applyDirMeta(dirs[i].path, dirs[i].hdr, opts)
	}
	return stats, nil
}

// cleanEntryPath validates and normalises a tar entry name, returning "" for
// entries that carry no path.
func cleanEntryPath(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("layer entry %q contains a NUL byte", name)
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("layer entry %q uses an absolute path", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == "/" {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("layer entry %q escapes the layer root", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("layer entry %q escapes the layer root", name)
		}
	}
	return clean, nil
}

// secureJoin joins rel onto dest after verifying that no existing parent
// component is a symlink pointing outside dest.
func secureJoin(dest, rel string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(rel))
	if !within(dest, target) {
		return "", fmt.Errorf("layer entry %q escapes the layer root", rel)
	}
	parent := filepath.Dir(target)
	resolved, err := resolveParent(dest, parent)
	if err != nil {
		return "", err
	}
	if !within(dest, resolved) {
		return "", fmt.Errorf("layer entry %q escapes the layer root through a symlink", rel)
	}
	return filepath.Join(resolved, filepath.Base(target)), nil
}

// resolveParent resolves symlinks in the parent directory, ensuring the result
// stays inside dest.
func resolveParent(dest, parent string) (string, error) {
	rel, err := filepath.Rel(dest, parent)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return dest, nil
	}
	cur := dest
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Remaining components do not exist yet: nothing can escape.
				return cur, nil
			}
			return "", fmt.Errorf("inspect %s: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", fmt.Errorf("resolve symlink %s: %w", cur, err)
			}
			if !within(dest, resolved) {
				return "", fmt.Errorf("layer entry escapes the layer root through symlink %s", cur)
			}
			cur = resolved
		}
	}
	return cur, nil
}

// within reports whether child is dest itself or lives under it.
func within(dest, child string) bool {
	rel, err := filepath.Rel(dest, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func makeDir(target string, hdr *tar.Header) error {
	info, err := os.Lstat(target)
	if err == nil {
		if !info.IsDir() {
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("replace %s with a directory: %w", target, err)
			}
		} else {
			return os.Chmod(target, os.FileMode(hdr.Mode)&0o7777)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o7777); err != nil {
		return fmt.Errorf("create directory %s: %w", target, err)
	}
	return os.Chmod(target, os.FileMode(hdr.Mode)&0o7777)
}

func writeRegularFile(target string, hdr *tar.Header, r io.Reader, opts UnpackOptions) (int64, error) {
	mode := os.FileMode(hdr.Mode) & 0o7777
	fd, err := unix.Open(target, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode))
	if err == unix.ELOOP {
		// The path exists as a symlink: the layer is replacing it with a file.
		if err := os.Remove(target); err != nil {
			return 0, fmt.Errorf("replace symlink %s: %w", target, err)
		}
		fd, err = unix.Open(target, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode))
	}
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", target, err)
	}
	f := os.NewFile(uintptr(fd), target)
	defer f.Close()

	var written int64
	var reader io.Reader = r
	if opts.MaxBytes > 0 {
		reader = io.LimitReader(r, opts.MaxBytes+1)
	}
	n, err := io.Copy(f, reader)
	written = n
	if err != nil {
		return written, fmt.Errorf("write %s: %w", target, err)
	}
	if opts.MaxBytes > 0 && written > opts.MaxBytes {
		return written, fmt.Errorf("layer exceeds the maximum unpacked size (%d bytes)", opts.MaxBytes)
	}
	applyXattrs(fd, hdr)
	return written, nil
}

func replaceSymlink(target, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("symlink %s has an empty target", target)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	if err := os.Symlink(linkname, target); err != nil {
		return fmt.Errorf("create symlink %s -> %s: %w", target, linkname, err)
	}
	return nil
}

func replaceWithHardlink(target, linkTarget string) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	if err := os.Link(linkTarget, target); err != nil {
		return fmt.Errorf("create hardlink %s -> %s: %w", target, linkTarget, err)
	}
	return nil
}

func makeDevice(target string, hdr *tar.Header) error {
	mode := uint32(hdr.Mode & 0o7777)
	switch hdr.Typeflag {
	case tar.TypeChar:
		mode |= unix.S_IFCHR
	case tar.TypeBlock:
		mode |= unix.S_IFBLK
	}
	dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	if err := unix.Mknod(target, mode, int(dev)); err != nil {
		if err == unix.EPERM {
			// Unprivileged extraction cannot create device nodes. Skip the
			// entry rather than failing the whole layer.
			return nil
		}
		return fmt.Errorf("create device %s: %w", target, err)
	}
	return nil
}

func replaceFifo(target string, hdr *tar.Header) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	if err := unix.Mkfifo(target, uint32(hdr.Mode&0o7777)); err != nil {
		if err == unix.EPERM {
			return nil
		}
		return fmt.Errorf("create fifo %s: %w", target, err)
	}
	return nil
}

// clearDir removes the contents of dest/rel, skipping paths this layer created.
func clearDir(dest, rel string, created map[string]bool) error {
	dir := filepath.Join(dest, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		childRel := path.Join(rel, e.Name())
		if created[childRel] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// removeEntry deletes dest/rel (a whiteout target).
func removeEntry(dest, rel string) error {
	target := filepath.Join(dest, filepath.FromSlash(rel))
	if !within(dest, target) {
		return fmt.Errorf("whiteout target %q escapes the layer root", rel)
	}
	if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func chown(target string, hdr *tar.Header) {
	// Ownership is best effort: unprivileged extraction keeps the current user.
	_ = os.Lchown(target, hdr.Uid, hdr.Gid)
}

func applyDirMeta(target string, hdr *tar.Header, opts UnpackOptions) {
	if !opts.SkipChown {
		chown(target, hdr)
	}
	_ = os.Chmod(target, os.FileMode(hdr.Mode)&0o7777)
	if !hdr.ModTime.IsZero() {
		_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
	}
}

// applyXattrs copies the extended attributes carried by a tar entry.
func applyXattrs(fd int, hdr *tar.Header) {
	for k, v := range hdr.PAXRecords {
		const prefix = "SCHILY.xattr."
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name := strings.TrimPrefix(k, prefix)
		_ = unix.Fsetxattr(fd, name, []byte(v), 0)
	}
}
