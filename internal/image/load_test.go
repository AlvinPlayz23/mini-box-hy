package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

// entry describes one file in a synthetic archive.
type entry struct {
	name     string
	typeflag byte
	body     string
	linkname string
	mode     int64
}

func writeTar(t *testing.T, w *tar.Writer, entries []entry) {
	t.Helper()
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Size: int64(len(e.body)), Linkname: e.linkname}
		if typ == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func writeTarGz(t *testing.T, path string, entries []entry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	writeTar(t, tw, entries)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func testImageStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv(container.RootEnv, root)
	cs, err := container.OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	return OpenStore(cs)
}

func TestLoadRootfsTarGz(t *testing.T) {
	store := testImageStore(t)
	archive := filepath.Join(t.TempDir(), "rootfs.tar.gz")
	writeTarGz(t, archive, []entry{
		{name: "bin/", typeflag: tar.TypeDir},
		{name: "bin/hello", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/motd", body: "welcome\n"},
	})

	rec, err := LoadArchive(context.Background(), store, archive, LoadOptions{Tag: "testimg:latest"})
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if len(rec.Layers) != 1 {
		t.Fatalf("layers = %d, want 1", len(rec.Layers))
	}
	if rec.Layers[0].MediaType != MediaTypeLayerGzip {
		t.Errorf("media type = %q", rec.Layers[0].MediaType)
	}
	chainDir := store.LayerDir(rec.ChainID)
	data, err := os.ReadFile(filepath.Join(chainDir, "etc/motd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "welcome\n" {
		t.Errorf("motd = %q", data)
	}
	info, err := os.Stat(filepath.Join(chainDir, "bin/hello"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("bin/hello mode = %o", info.Mode().Perm())
	}
	if _, err := store.ResolveRecord("testimg"); err != nil {
		t.Fatalf("ResolveRecord: %v", err)
	}
}

func TestLoadUncompressedRootfsTar(t *testing.T) {
	store := testImageStore(t)
	archive := filepath.Join(t.TempDir(), "rootfs.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	writeTar(t, tw, []entry{{name: "etc/hostname", body: "box\n"}})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadArchive(context.Background(), store, archive, LoadOptions{Tag: "plain:1"})
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if rec.Layers[0].MediaType != MediaTypeLayerUncompressed {
		t.Errorf("media type = %q, want uncompressed", rec.Layers[0].MediaType)
	}
	if _, err := os.Stat(filepath.Join(store.LayerDir(rec.ChainID), "etc/hostname")); err != nil {
		t.Error("rootfs content missing")
	}
}

func TestLoadDockerSaveArchive(t *testing.T) {
	store := testImageStore(t)

	// Layer 1: base files. Layer 2: adds a file and whiteouts another.
	var layer1 bytes.Buffer
	tw1 := tar.NewWriter(&layer1)
	writeTar(t, tw1, []entry{
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/keep", body: "keep"},
		{name: "etc/remove-me", body: "gone"},
	})
	if err := tw1.Close(); err != nil {
		t.Fatal(err)
	}
	var layer2 bytes.Buffer
	tw2 := tar.NewWriter(&layer2)
	writeTar(t, tw2, []entry{
		{name: "etc/.wh.remove-me"},
		{name: "etc/added", body: "added"},
	})
	if err := tw2.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := ConfigFile{
		Architecture: CurrentPlatform().Architecture,
		OS:           CurrentPlatform().OS,
		Config:       ImageConfig{Env: []string{"FOO=bar"}, Cmd: []string{"/bin/echo", "hi"}},
	}
	cfgData, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []dockerSaveManifest{{
		Config:   "abc/config.json",
		RepoTags: []string{"saved:1"},
		Layers:   []string{"abc/layer1.tar", "abc/layer2.tar"},
	}}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "save.tar")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	outer := tar.NewWriter(f)
	for _, e := range []entry{
		{name: "abc/layer1.tar", body: layer1.String()},
		{name: "abc/layer2.tar", body: layer2.String()},
		{name: "abc/config.json", body: string(cfgData)},
		{name: "manifest.json", body: string(manifestData)},
	} {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}
		if err := outer.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := outer.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := outer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rec, err := LoadArchive(context.Background(), store, archive, LoadOptions{Tag: "saved:1"})
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if len(rec.Layers) != 2 {
		t.Fatalf("layers = %d, want 2", len(rec.Layers))
	}
	if rec.ConfigDigest == "" {
		t.Error("config digest was not recorded")
	}
	if len(rec.Config.Config.Env) != 1 || rec.Config.Config.Env[0] != "FOO=bar" {
		t.Errorf("image config not imported: %+v", rec.Config.Config)
	}
	chainDir := store.LayerDir(rec.ChainID)
	if _, err := os.Stat(filepath.Join(chainDir, "etc/added")); err != nil {
		t.Error("second layer was not applied")
	}
	if _, err := os.Stat(filepath.Join(chainDir, "etc/remove-me")); !os.IsNotExist(err) {
		t.Error("whiteout from the second layer was not applied")
	}
	if _, err := os.Stat(filepath.Join(chainDir, "etc/keep")); err != nil {
		t.Error("file from the first layer disappeared")
	}
}

func TestLoadMissingFile(t *testing.T) {
	store := testImageStore(t)
	if _, err := LoadArchive(context.Background(), store, filepath.Join(t.TempDir(), "nope.tar"), LoadOptions{}); err == nil {
		t.Fatal("expected an error for a missing archive")
	}
}

func TestLoadDefaultTagFromFilename(t *testing.T) {
	store := testImageStore(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "myrootfs.tar.gz")
	writeTarGz(t, archive, []entry{{name: "etc/hostname", body: "x"}})
	rec, err := LoadArchive(context.Background(), store, archive, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ref != "docker.io/library/myrootfs:latest" {
		t.Errorf("ref = %q", rec.Ref)
	}
}
