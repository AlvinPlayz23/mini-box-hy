//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildOverlayOptionsShortPaths(t *testing.T) {
	lower := []string{"/a/b", "/c/d"}
	opts, fds, err := buildOverlayOptions(lower, "/upper", "/work", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(fds) != 0 {
		t.Errorf("short options should not need fds, got %d", len(fds))
	}
	want := "lowerdir=/a/b:/c/d,upperdir=/upper,workdir=/work"
	if opts != want {
		t.Errorf("options = %q, want %q", opts, want)
	}
}

func TestBuildOverlayOptionsRootless(t *testing.T) {
	opts, _, err := buildOverlayOptions([]string{"/a"}, "/u", "/w", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opts, "userxattr") {
		t.Errorf("rootless options %q lack userxattr", opts)
	}
}

func TestBuildOverlayOptionsLongChainUsesFDPaths(t *testing.T) {
	base := t.TempDir()
	var lower []string
	for i := 0; i < 60; i++ {
		dir := filepath.Join(base, strings.Repeat("l", 60), strings.Repeat("x", 40)+string(rune('a'+i%26))+strings.Repeat("y", 20))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		lower = append(lower, dir)
	}
	opts, fds, err := buildOverlayOptions(lower, filepath.Join(base, "upper"), filepath.Join(base, "work"), false)
	if err != nil {
		t.Fatalf("buildOverlayOptions: %v", err)
	}
	if len(fds) != len(lower) {
		t.Fatalf("got %d fds for %d layers", len(fds), len(lower))
	}
	if len(opts) > overlayMaxOptionLen {
		t.Errorf("options still too long: %d bytes", len(opts))
	}
	if !strings.Contains(opts, "/proc/self/fd/") {
		t.Errorf("options do not use fd paths: %q", opts)
	}
	for _, fd := range fds {
		if err := unixClose(fd); err != nil {
			t.Errorf("close fd %d: %v", fd, err)
		}
	}
}

func TestParseMountInfo(t *testing.T) {
	data := `21 28 0:20 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
30 21 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:14 - proc proc rw
42 21 0:33 / /var/lib/minibox/containers/abc/merged rw,relatime shared:1 - overlay overlay rw,lowerdir=/a:/b,upperdir=/c,workdir=/d
57 30 0:24 / /proc/with\040space rw - tmpfs tmpfs rw
`
	mounts, err := ParseMountInfo(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 4 {
		t.Fatalf("parsed %d mounts, want 4", len(mounts))
	}
	ov := mounts[2]
	if ov.FSType != "overlay" || ov.MountPoint != "/var/lib/minibox/containers/abc/merged" {
		t.Errorf("overlay record = %+v", ov)
	}
	if mounts[3].MountPoint != "/proc/with space" {
		t.Errorf("octal escape not decoded: %q", mounts[3].MountPoint)
	}

	points := MountedPoints(mounts, "/var/lib/minibox")
	if len(points) != 1 || points[0] != "/var/lib/minibox/containers/abc/merged" {
		t.Errorf("MountedPoints = %v", points)
	}
	if got := MountedPoints(mounts, "/nonexistent"); len(got) != 0 {
		t.Errorf("MountedPoints for unrelated prefix = %v", got)
	}
}

func TestPrepareRootfsWithoutOverlay(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "layer")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(dir, "container")
	if err := os.MkdirAll(containerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := PrepareRootfs(containerDir, []string{layer}, RootfsOptions{Overlay: false})
	if err != nil {
		t.Fatal(err)
	}
	if p.Merged != layer {
		t.Errorf("Merged = %q, want %q", p.Merged, layer)
	}
	if err := TeardownRootfs(p); err != nil {
		t.Errorf("TeardownRootfs: %v", err)
	}
	if _, err := os.Stat(layer); err != nil {
		t.Error("teardown removed the shared layer directory")
	}
}

func TestPrepareRootfsRejectsMissingLayer(t *testing.T) {
	dir := t.TempDir()
	if _, err := PrepareRootfs(dir, []string{filepath.Join(dir, "nope")}, RootfsOptions{}); err == nil {
		t.Fatal("expected an error for a missing layer")
	}
	if _, err := PrepareRootfs(dir, nil, RootfsOptions{}); err == nil {
		t.Fatal("expected an error for an empty layer list")
	}
}

func TestPrepareRootfsWithoutOverlayRejectsMultipleLayers(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PrepareRootfs(dir, []string{a, b}, RootfsOptions{Overlay: false}); err == nil {
		t.Fatal("expected an error when overlay is disabled with several layers")
	}
}
