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
