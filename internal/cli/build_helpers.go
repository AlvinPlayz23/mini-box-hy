//go:build linux

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlvinPlayz23/minibox/internal/build"
)

// runArgv renders a RUN instruction as an argv.
func runArgv(ins build.Instruction, vars map[string]string) ([]string, error) {
	if ins.Exec != nil {
		argv := make([]string, 0, len(ins.Exec))
		for _, a := range ins.Exec {
			expanded, err := build.ExpandArgs(a, vars)
			if err != nil {
				return nil, err
			}
			argv = append(argv, expanded)
		}
		return argv, nil
	}
	script, err := build.ExpandArgs(ins.Args, vars)
	if err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", script}, nil
}

// execFormArgv renders CMD/ENTRYPOINT, which may be shell form.
func execFormArgv(ins build.Instruction, vars map[string]string) ([]string, error) {
	if ins.Exec != nil {
		argv := make([]string, 0, len(ins.Exec))
		for _, a := range ins.Exec {
			expanded, err := build.ExpandArgs(a, vars)
			if err != nil {
				return nil, err
			}
			argv = append(argv, expanded)
		}
		return argv, nil
	}
	script, err := build.ExpandArgs(ins.Args, vars)
	if err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", script}, nil
}

// copyPatterns returns the source patterns of a COPY instruction, expanded, so
// the cache can hash exactly the files that step consumes.
func copyPatterns(ins build.Instruction, vars map[string]string) ([]string, error) {
	fields := splitFields(ins.Args)
	if len(fields) < 2 {
		return nil, fmt.Errorf("%s needs at least a source and a destination", ins.Cmd)
	}
	var out []string
	for _, pattern := range fields[:len(fields)-1] {
		expanded, err := build.ExpandArgs(pattern, vars)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// splitFields splits a shell-form argument on whitespace, respecting simple
// quoting, which is all the Dockerfile subset needs.
func splitFields(s string) []string {
	var out []string
	var current strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				continue
			}
			current.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// parseKeyValues parses "KEY=value KEY2=value2" instruction arguments, also
// accepting the quoted form ENV KEY="value".
func parseKeyValues(args string) map[string]string {
	out := map[string]string{}
	fields := splitFields(args)
	// ENV supports both "KEY value" (two tokens) and "KEY=value" forms.
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if key, value, ok := strings.Cut(field, "="); ok {
			out[key] = strings.Trim(value, `"'`)
			continue
		}
		if i+1 < len(fields) && !strings.ContainsRune(fields[i+1], '=') {
			out[field] = strings.Trim(fields[i+1], `"'`)
			i++
		}
	}
	return out
}

// copyInto copies a file, directory or symlink into the build workspace,
// preserving modes and refusing to follow symlinks out of the source.
func copyInto(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyInto(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode().Perm())
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	default:
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chmod(dst, info.Mode().Perm())
	}
}
