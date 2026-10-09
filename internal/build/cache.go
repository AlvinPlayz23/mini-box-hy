package build

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// CacheKey returns a stable key for one build step, so that an unchanged
// instruction over an unchanged base and context reuses its layer.
func CacheKey(baseKey string, in Instruction, contextHash string) string {
	h := sha256.New()
	writeKeyField(h, baseKey)
	writeKeyField(h, in.Cmd)
	writeKeyField(h, in.Args)
	if in.Exec != nil {
		writeKeyField(h, "exec")
		for _, e := range in.Exec {
			writeKeyField(h, e)
		}
	} else {
		writeKeyField(h, "shell")
	}
	names := make([]string, 0, len(in.Flags))
	for name := range in.Flags {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeKeyField(h, name)
		writeKeyField(h, in.Flags[name])
	}
	writeKeyField(h, contextHash)
	return hex.EncodeToString(h.Sum(nil))
}

// writeKeyField appends a NUL-separated field to the key hash.
func writeKeyField(h io.Writer, s string) {
	io.WriteString(h, s)
	h.Write([]byte{0})
}

// ContextHash hashes the files a COPY/ADD instruction would use, so that
// touching an unrelated file does not invalidate the cache.
func ContextHash(contextDir string, patterns []string) (string, error) {
	files, dirs, err := walkContext(contextDir)
	if err != nil {
		return "", err
	}
	matched := make(map[string]bool)
	for _, pat := range patterns {
		if strings.TrimSpace(pat) == "" {
			return "", errors.New("context hash: empty pattern")
		}
		clean := path.Clean(filepath.ToSlash(pat))
		n := 0
		for _, p := range files {
			ok, err := contextMatch(clean, p)
			if err != nil {
				return "", err
			}
			if ok {
				matched[p] = true
				n++
			}
		}
		for _, d := range dirs {
			ok, err := contextMatch(clean, d)
			if err != nil {
				return "", err
			}
			if ok {
				matched[d] = true
				n++
			}
		}
		if n == 0 {
			return "", fmt.Errorf("context hash: pattern %q matched nothing in %s", pat, contextDir)
		}
	}
	h := sha256.New()
	for _, f := range files {
		if !matched[f] && !hasMatchedAncestor(f, matched) {
			continue
		}
		info, err := os.Lstat(filepath.Join(contextDir, filepath.FromSlash(f)))
		if err != nil {
			return "", err
		}
		if err := hashContextFile(h, contextDir, f, info); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// walkContext returns the slash-separated relative paths of every file
// (including symlinks) and directory under contextDir, in lexical order.
func walkContext(contextDir string) (files, dirs []string, err error) {
	err = filepath.WalkDir(contextDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(contextDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
		} else {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk build context %s: %w", contextDir, err)
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs, nil
}

// contextMatch reports whether rel matches a context pattern: an exact path,
// a filepath.Match pattern such as "build/*", or "." for everything.
func contextMatch(pattern, rel string) (bool, error) {
	if pattern == "." {
		return true, nil
	}
	ok, err := filepath.Match(pattern, rel)
	if err != nil {
		return false, fmt.Errorf("context pattern %q: %w", pattern, err)
	}
	return ok, nil
}

// hasMatchedAncestor reports whether a directory above rel matched, which
// pulls the whole subtree of a matched directory into the hash.
func hasMatchedAncestor(rel string, matched map[string]bool) bool {
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		if matched[dir] {
			return true
		}
		if dir == "." || dir == "/" {
			return false
		}
	}
}

// hashContextFile hashes the relative path, size, mode and content of one file.
func hashContextFile(h io.Writer, contextDir, rel string, info os.FileInfo) error {
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00", rel, info.Size(), uint32(info.Mode()))
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(filepath.Join(contextDir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		io.WriteString(h, target)
	case info.Mode().IsRegular():
		data, err := os.ReadFile(filepath.Join(contextDir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		h.Write(data)
	}
	h.Write([]byte{0})
	return nil
}
