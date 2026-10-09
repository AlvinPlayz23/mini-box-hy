package build

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ExpandCopySources resolves COPY sources relative to the build context,
// refusing absolute paths, ".." escapes and paths that resolve outside the
// context through symlinks. Patterns support a single trailing "*" glob.
func ExpandCopySources(contextDir, pattern string) ([]string, error) {
	absCtx, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, err
	}
	pat := strings.TrimSpace(pattern)
	if pat == "" {
		return nil, errors.New("COPY source pattern is empty")
	}
	pat = path.Clean(filepath.ToSlash(pat))
	if path.IsAbs(pat) {
		return nil, fmt.Errorf("COPY source %q must be relative to the build context", pattern)
	}
	if pat == ".." || strings.HasPrefix(pat, "../") {
		return nil, fmt.Errorf("COPY source %q escapes the build context", pattern)
	}
	if strings.Contains(pat, "*") {
		return expandCopyGlob(absCtx, pattern, pat)
	}
	abs := filepath.Join(absCtx, filepath.FromSlash(pat))
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("COPY source %q not found in the build context", pattern)
		}
		return nil, err
	}
	if _, err := resolveWithin(absCtx, abs); err != nil {
		return nil, err
	}
	return []string{pat}, nil
}

// expandCopyGlob expands a glob pattern via filepath.Glob and refuses matches
// that resolve outside the build context.
func expandCopyGlob(absCtx, origPattern, pat string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(absCtx, filepath.FromSlash(pat)))
	if err != nil {
		return nil, fmt.Errorf("COPY source %q: %w", origPattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("COPY source %q matched no files in the build context", origPattern)
	}
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, err := resolveWithin(absCtx, m); err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(absCtx, m)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// resolveWithin resolves absTarget under dir, evaluating symlinks on the
// deepest existing ancestor and refusing paths that leave dir.
func resolveWithin(dir, absTarget string) (string, error) {
	rel, err := filepath.Rel(dir, absTarget)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return dir, nil
	}
	cur := dir
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		next := filepath.Join(cur, part)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			// Remaining components do not exist yet: nothing can escape.
			return cur, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", err
			}
			if !staysWithin(dir, resolved) {
				return "", fmt.Errorf("COPY source %q escapes the build context through symlink %s", filepath.ToSlash(rel), next)
			}
			next = resolved
		}
		cur = next
	}
	return cur, nil
}

// staysWithin reports whether target is dir itself or inside it.
func staysWithin(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
