package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

func TestPullMultiLayerImage(t *testing.T) {
	if os.Getenv("MINIBOX_TEST_LARGE_IMAGES") == "" {
		t.Skip("set MINIBOX_TEST_LARGE_IMAGES=1 to pull a multi-layer image")
	}
	root := t.TempDir()
	t.Setenv(container.RootEnv, root)
	cs, err := container.OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	store := OpenStore(cs)
	ref, err := ParseReference("nginx:alpine")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rec, err := Pull(ctx, store, ref, PullOptions{Progress: func(m string) { t.Log(m) }})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(rec.Layers) < 3 {
		t.Fatalf("nginx:alpine should have several layers, got %d", len(rec.Layers))
	}
	chainDir := store.LayerDir(rec.ChainID)
	for _, p := range []string{"usr/sbin/nginx", "etc/nginx/nginx.conf", "etc/alpine-release"} {
		if _, err := os.Lstat(filepath.Join(chainDir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	blobs, layers, err := store.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disk usage: blobs=%d layers=%d", blobs, layers)
}

// TestPullImageMatrix pulls a spread of real images: minimal single-layer,
// apt-based, and multi-layer. Set MINIBOX_TEST_AI_IMAGES=1 to add the very
// large AI image (several GiB), which needs disk and bandwidth.
func TestPullImageMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped with -short")
	}
	images := []struct {
		ref   string
		probe string // a path that must exist in the unpacked rootfs
	}{
		{"busybox:1.36", "bin/busybox"},
		{"debian:stable-slim", "etc/os-release"},
		{"python:3.12-slim", "usr/local/bin/python3.12"},
	}
	if os.Getenv("MINIBOX_TEST_AI_IMAGES") != "" {
		images = append(images, struct {
			ref   string
			probe string
		}{"ollama/ollama:latest", "usr/bin/ollama"})
	}
	for _, tc := range images {
		t.Run(tc.ref, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(container.RootEnv, root)
			cs, err := container.OpenAt(root)
			if err != nil {
				t.Fatal(err)
			}
			store := OpenStore(cs)
			ref, err := ParseReference(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			start := time.Now()
			rec, err := Pull(ctx, store, ref, PullOptions{})
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}
			elapsed := time.Since(start)
			if _, err := os.Lstat(filepath.Join(store.LayerDir(rec.ChainID), tc.probe)); err != nil {
				t.Errorf("unpacked rootfs is missing %s: %v", tc.probe, err)
			}
			blobs, layers, err := store.DiskUsage()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d layers, %s wall, blobs %d MB, layers %d MB",
				tc.ref, len(rec.Layers), elapsed.Round(time.Millisecond),
				blobs>>20, layers>>20)
		})
	}
}
