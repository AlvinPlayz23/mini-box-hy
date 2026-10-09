package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

// networkTestStore prepares a store in a temp dir, as the working agreements
// require for every test.
func networkTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	t.Setenv(container.RootEnv, root)
	cs, err := container.OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	return OpenStore(cs)
}

func TestPullAlpineFromDockerHub(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped with -short")
	}
	store := networkTestStore(t)
	ref, err := ParseReference("alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rec, err := Pull(ctx, store, ref, PullOptions{Progress: func(msg string) { t.Log(msg) }})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(rec.Layers) == 0 {
		t.Fatal("record has no layers")
	}
	if rec.Config.OS != "linux" {
		t.Errorf("image OS = %q, want linux", rec.Config.OS)
	}
	if rec.Config.Architecture == "" {
		t.Error("image architecture is empty")
	}

	chainDir := store.LayerDir(rec.ChainID)
	// A usable Alpine root filesystem: the dynamic loader and the shell.
	for _, path := range []string{"bin/sh", "etc/os-release", "bin/busybox"} {
		if _, err := os.Lstat(filepath.Join(chainDir, path)); err != nil {
			t.Errorf("unpacked rootfs is missing %s: %v", path, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(chainDir, "etc/os-release"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "Alpine") {
		t.Errorf("etc/os-release does not mention Alpine:\n%s", data)
	}

	// Every blob must hash to its name.
	for _, l := range rec.Layers {
		if !store.HasBlob(l.Digest) {
			t.Errorf("layer blob %s is missing", l.Digest)
		}
		checkBlobDigest(t, store.BlobPath(l.Digest), l.Digest)
	}

	// A second pull must reuse the unpacked chain instead of re-downloading.
	rec2, err := Pull(ctx, store, ref, PullOptions{})
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if rec2.ChainID != rec.ChainID {
		t.Errorf("chain id changed between pulls: %s != %s", rec2.ChainID, rec.ChainID)
	}
	records, err := store.ListRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Errorf("store holds %d records after pulling the same tag twice, want 1", len(records))
	}
}

func TestPullByDigest(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped with -short")
	}
	store := networkTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Resolve the tag, then pull the same image by its digest.
	ref, err := ParseReference("alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Pull(ctx, store, ref, PullOptions{})
	if err != nil {
		t.Fatalf("Pull by tag: %v", err)
	}
	digestRef, err := ParseReference("alpine@" + rec.Digest)
	if err != nil {
		t.Fatal(err)
	}
	byDigest, err := Pull(ctx, store, digestRef, PullOptions{})
	if err != nil {
		t.Fatalf("Pull by digest: %v", err)
	}
	if byDigest.Digest != rec.Digest {
		t.Errorf("digest pull resolved to %s, want %s", byDigest.Digest, rec.Digest)
	}
}

func TestPullMissingImageFailsClearly(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped with -short")
	}
	store := networkTestStore(t)
	ref, err := ParseReference("alpine:this-tag-does-not-exist-minibox")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := Pull(ctx, store, ref, PullOptions{}); err == nil {
		t.Fatal("pulling a nonexistent tag succeeded")
	}
}

func checkBlobDigest(t *testing.T, path, want string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := VerifyDigest(f, want); err != nil {
		t.Errorf("blob %s: %v", path, err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
