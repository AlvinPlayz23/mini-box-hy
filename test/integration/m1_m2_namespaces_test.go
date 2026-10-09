//go:build integration

package integration

import (
	"strings"
	"testing"
)

// M1: a container sees its own hostname and PID 1, and nothing leaks.
func TestM1RunRawIsolation(t *testing.T) {
	requireRoot(t)
	rootfs := rootfsFor(t)
	out, errOut, code := mb(t, "run-raw", "--hostname", "rawbox", rootfs, "/bin/sh", "-c", "hostname; echo $$; ls /proc | grep -c '^[0-9]*$'")
	if code != 0 {
		t.Fatalf("run-raw failed (%d): %s%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected output: %q", out)
	}
	if lines[0] != "rawbox" {
		t.Errorf("hostname = %q, want rawbox", lines[0])
	}
	if lines[1] != "1" {
		t.Errorf("shell pid = %q, want 1 (a new PID namespace)", lines[1])
	}
	if mounts := leakedMounts(t); len(mounts) > 0 {
		t.Errorf("leaked mounts after run-raw: %v", mounts)
	}
}

// M2: pivot_root, /dev, /proc and read-only /sys behave as expected.
func TestM2RootFilesystem(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	script := strings.Join([]string{
		"test -e /.minibox-oldroot && echo OLDROOT_VISIBLE || echo OLDROOT_GONE",
		"echo data > /dev/null && echo DEVNULL_OK",
		"head -c 16 /dev/urandom >/dev/null && echo URANDOM_OK",
		"touch /sys/minibox-probe 2>/dev/null && echo SYS_WRITABLE || echo SYS_READONLY",
		"ls /proc/1/cmdline >/dev/null 2>&1 && echo PROC_PID1_OK",
	}, "; ")
	out, errOut, code := mb(t, "run", "--rm", "--network", "none", testImage, "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("run failed (%d): %s%s", code, out, errOut)
	}
	for _, want := range []string{"OLDROOT_GONE", "DEVNULL_OK", "URANDOM_OK", "SYS_READONLY", "PROC_PID1_OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s%s", want, out, errOut)
		}
	}
	if mounts := leakedMounts(t); len(mounts) > 0 {
		t.Errorf("leaked mounts after run: %v", mounts)
	}
}
