//go:build linux

package storage

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry describes one file to place in a test layer.
type tarEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typ,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
			Uid:      os.Getuid(),
			Gid:      os.Getgid(),
		}
		if typ == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			// Names that archive/tar refuses to encode (for example a trailing
			// slash on a regular file) are not interesting inputs.
			continue
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func unpack(t *testing.T, dest string, entries []tarEntry) UnpackStats {
	t.Helper()
	stats, err := UnpackTar(bytes.NewReader(buildTar(t, entries)), UnpackOptions{Dest: dest, SkipChown: true})
	if err != nil {
		t.Fatalf("UnpackTar: %v", err)
	}
	return stats
}

func TestUnpackBasicTree(t *testing.T) {
	dest := t.TempDir()
	stats := unpack(t, dest, []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/hostname", body: "box\n", mode: 0o644},
		{name: "bin/", typeflag: tar.TypeDir},
		{name: "bin/sh", body: "#!/bin/sh\n", mode: 0o755},
		{name: "bin/sh-link", typeflag: tar.TypeSymlink, linkname: "/bin/sh"},
		{name: "bin/sh-hard", typeflag: tar.TypeLink, linkname: "bin/sh"},
	})
	if stats.Files != 2 || stats.Dirs != 2 || stats.Symlinks != 1 || stats.Hardlinks != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	data, err := os.ReadFile(filepath.Join(dest, "etc/hostname"))
	if err != nil || string(data) != "box\n" {
		t.Errorf("etc/hostname = %q, %v", data, err)
	}
	info, err := os.Lstat(filepath.Join(dest, "bin/sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("bin/sh mode = %o, want 755", info.Mode().Perm())
	}
	if target, err := os.Readlink(filepath.Join(dest, "bin/sh-link")); err != nil || target != "/bin/sh" {
		t.Errorf("symlink target = %q, %v", target, err)
	}
	hard, err := os.Stat(filepath.Join(dest, "bin/sh-hard"))
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Stat(filepath.Join(dest, "bin/sh"))
	if !os.SameFile(hard, orig) {
		t.Error("hardlink does not share an inode")
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"dotdot", []tarEntry{{name: "../evil", body: "x"}}},
		{"nested dotdot", []tarEntry{{name: "a/../../evil", body: "x"}}},
		{"absolute", []tarEntry{{name: "/evil", body: "x"}}},
		{"absolute dir", []tarEntry{{name: "/etc/passwd", body: "x"}}},
		{"whiteout traversal", []tarEntry{{name: "../.wh.evil"}}},
		{"hardlink escape", []tarEntry{
			{name: "ok", body: "x"},
			{name: "bad", typeflag: tar.TypeLink, linkname: "../../etc/passwd"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "root")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := UnpackTar(bytes.NewReader(buildTar(t, tc.entries)), UnpackOptions{Dest: dest, SkipChown: true})
			if err == nil {
				t.Fatal("expected an error, extraction succeeded")
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "root" {
					t.Errorf("extraction created %q outside the layer root", e.Name())
				}
			}
		})
	}
}

func TestUnpackRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []tarEntry{
		{name: "escape", typeflag: tar.TypeSymlink, linkname: outside},
		{name: "escape/owned", body: "pwned"},
	}
	if _, err := UnpackTar(bytes.NewReader(buildTar(t, entries)), UnpackOptions{Dest: dest, SkipChown: true}); err == nil {
		t.Fatal("expected extraction through a symlink to fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "owned")); err == nil {
		t.Fatal("file was written outside the layer root")
	}
}

func TestUnpackWhiteouts(t *testing.T) {
	dest := t.TempDir()
	// Base layer.
	unpack(t, dest, []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/keep", body: "keep"},
		{name: "etc/gone", body: "gone"},
		{name: "usr/", typeflag: tar.TypeDir},
		{name: "usr/bin/", typeflag: tar.TypeDir},
		{name: "usr/bin/tool", body: "tool"},
	})
	// Layer that deletes etc/gone and the whole usr/bin directory.
	stats := unpack(t, dest, []tarEntry{
		{name: "etc/.wh.gone"},
		{name: "usr/.wh.bin"},
		{name: "etc/new", body: "new"},
	})
	if stats.Whiteouts != 2 {
		t.Errorf("whiteouts = %d, want 2", stats.Whiteouts)
	}
	if _, err := os.Stat(filepath.Join(dest, "etc/gone")); !os.IsNotExist(err) {
		t.Error("etc/gone was not removed")
	}
	if _, err := os.Stat(filepath.Join(dest, "etc/keep")); err != nil {
		t.Error("etc/keep should survive")
	}
	if _, err := os.Stat(filepath.Join(dest, "usr/bin")); !os.IsNotExist(err) {
		t.Error("usr/bin was not removed")
	}
	if _, err := os.Stat(filepath.Join(dest, "etc/new")); err != nil {
		t.Error("etc/new missing")
	}
}

func TestUnpackOpaqueDirectory(t *testing.T) {
	dest := t.TempDir()
	unpack(t, dest, []tarEntry{
		{name: "var/", typeflag: tar.TypeDir},
		{name: "var/log/", typeflag: tar.TypeDir},
		{name: "var/log/old.log", body: "old"},
		{name: "var/lib/", typeflag: tar.TypeDir},
		{name: "var/lib/keep", body: "keep"},
	})
	unpack(t, dest, []tarEntry{
		{name: "var/log/.wh..wh..opq"},
		{name: "var/log/new.log", body: "new"},
	})
	if _, err := os.Stat(filepath.Join(dest, "var/log/old.log")); !os.IsNotExist(err) {
		t.Error("opaque marker did not hide old.log")
	}
	if _, err := os.Stat(filepath.Join(dest, "var/log/new.log")); err != nil {
		t.Error("new.log missing after opaque marker")
	}
	if _, err := os.Stat(filepath.Join(dest, "var/lib/keep")); err != nil {
		t.Error("opaque marker leaked into a sibling directory")
	}
}

func TestUnpackOpaqueKeepsFilesFromSameLayer(t *testing.T) {
	dest := t.TempDir()
	unpack(t, dest, []tarEntry{
		{name: "d/", typeflag: tar.TypeDir},
		{name: "d/old", body: "old"},
	})
	// The marker appears after a file this same layer created.
	unpack(t, dest, []tarEntry{
		{name: "d/fresh", body: "fresh"},
		{name: "d/.wh..wh..opq"},
	})
	if _, err := os.Stat(filepath.Join(dest, "d/fresh")); err != nil {
		t.Error("file created by the same layer was removed by the opaque marker")
	}
	if _, err := os.Stat(filepath.Join(dest, "d/old")); !os.IsNotExist(err) {
		t.Error("opaque marker did not hide the lower file")
	}
}

func TestUnpackReplacesSymlinkWithFile(t *testing.T) {
	dest := t.TempDir()
	unpack(t, dest, []tarEntry{
		{name: "link", typeflag: tar.TypeSymlink, linkname: "/nonexistent"},
	})
	unpack(t, dest, []tarEntry{
		{name: "link", body: "now a file"},
	})
	info, err := os.Lstat(filepath.Join(dest, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("link is still a symlink")
	}
	data, _ := os.ReadFile(filepath.Join(dest, "link"))
	if string(data) != "now a file" {
		t.Errorf("content = %q", data)
	}
}

func TestUnpackMaxBytes(t *testing.T) {
	dest := t.TempDir()
	body := strings.Repeat("x", 4096)
	_, err := UnpackTar(bytes.NewReader(buildTar(t, []tarEntry{{name: "big", body: body}})),
		UnpackOptions{Dest: dest, SkipChown: true, MaxBytes: 1024})
	if err == nil {
		t.Fatal("expected the size limit to abort extraction")
	}
}

func TestChainIDIsStableAndOrdered(t *testing.T) {
	a := ChainID([]string{"sha256:1", "sha256:2"})
	b := ChainID([]string{"sha256:1", "sha256:2"})
	c := ChainID([]string{"sha256:2", "sha256:1"})
	if a != b {
		t.Error("chain id is not deterministic")
	}
	if a == c {
		t.Error("chain id ignores layer order")
	}
	if len(a) != 64 {
		t.Errorf("chain id length = %d, want 64", len(a))
	}
	if ChainID(nil) == "" {
		t.Error("empty chain should still hash")
	}
}

func TestCleanEntryPath(t *testing.T) {
	good := map[string]string{
		"etc/passwd":   "etc/passwd",
		"./etc/passwd": "etc/passwd",
		"a/b/../c":     "a/c",
		"dir/":         "dir",
		"./":           "",
		".":            "",
	}
	for in, want := range good {
		got, err := cleanEntryPath(in)
		if err != nil || got != want {
			t.Errorf("cleanEntryPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "/etc/passwd", "/", "a/\x00b"} {
		if _, err := cleanEntryPath(bad); err == nil {
			t.Errorf("cleanEntryPath(%q) succeeded, want error", bad)
		}
	}
}

func FuzzCleanEntryPath(f *testing.F) {
	for _, s := range []string{"etc/passwd", "../x", "a/../b", "/abs", "a/./b", "..", "a\x00b", "//x//y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := cleanEntryPath(s)
		if err != nil {
			return
		}
		if got == "" {
			return
		}
		if strings.HasPrefix(got, "/") || got == ".." || strings.HasPrefix(got, "../") {
			t.Fatalf("cleanEntryPath(%q) = %q escapes the root", s, got)
		}
	})
}

// FuzzUnpackStaysInDest fuzzes entry names and checks that nothing is ever
// created outside the destination directory.
func FuzzUnpackStaysInDest(f *testing.F) {
	for _, s := range []string{"etc/passwd", "../evil", "a/b", "x/../../y", "link", "/abs"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if strings.ContainsRune(name, 0) || len(name) > 200 {
			return
		}
		parent := t.TempDir()
		dest := filepath.Join(parent, "root")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		entries := []tarEntry{{name: name, body: "data"}}
		_, _ = UnpackTar(bytes.NewReader(buildTar(t, entries)), UnpackOptions{Dest: dest, SkipChown: true})

		err := filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == parent {
				return nil
			}
			if !within(dest, p) {
				t.Fatalf("entry %q created %s outside %s", name, p, dest)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

var _ = io.Discard
var _ = fmt.Sprintf
