//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// M4: two containers share the read-only layers; writes stay private; the
// second container costs almost no disk.
func TestM4OverlaySharingAndIsolation(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	out, _, code := mb(t, "run", "-d", "--network", "none", "--name", "writer", testImage,
		"/bin/sh", "-c", "echo hello > /tmp/shared-probe; sleep 30")
	if code != 0 {
		t.Fatalf("run -d writer failed: %s", out)
	}
	writer := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", writer)

	out, _, code = mb(t, "run", "-d", "--network", "none", "--name", "reader", testImage,
		"/bin/sh", "-c", "sleep 30")
	if code != 0 {
		t.Fatalf("run -d reader failed: %s", out)
	}
	reader := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", reader)

	time.Sleep(500 * time.Millisecond)
	if _, errOut, code := mb(t, "exec", writer, "sh", "-c", "test -f /tmp/shared-probe"); code != 0 {
		t.Fatalf("the writer's file is missing: %s", errOut)
	}
	if _, _, code := mb(t, "exec", reader, "sh", "-c", "test -f /tmp/shared-probe"); code == 0 {
		t.Error("a file written in one container is visible in another")
	}
	if _, _, code := mb(t, "exec", reader, "sh", "-c", "test -f /tmp/shared-probe"); code == 0 {
		t.Error("the image layer was modified")
	}

	// The second container's writable layer must be nearly empty.
	size := dirSizeBytes(t, filepath.Join(rootDir, "containers", reader, "upper"))
	if size > 1<<20 {
		t.Errorf("second container upper dir is %d bytes, expected close to zero", size)
	}
}

// M6: a clean store can run alpine end to end.
func TestM6RunFromCleanStore(t *testing.T) {
	requireRoot(t)
	requireNetwork(t)
	out, errOut, code := mb(t, "run", "--rm", "--network", "host", testImage, "echo", "hi")
	if code != 0 {
		t.Fatalf("run failed (%d): %s%s", code, out, errOut)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Errorf("output = %q, want hi", out)
	}
}

// M5: detached lifecycle - ps, logs, exec, stop, rm.
func TestM5DetachedLifecycle(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	out, errOut, code := mb(t, "run", "-d", "--network", "none", "--name", "lifecycle",
		testImage, "/bin/sh", "-c", "echo started; sleep 300")
	if code != 0 {
		t.Fatalf("run -d failed: %s%s", out, errOut)
	}
	id := strings.TrimSpace(out)
	if len(id) != 64 {
		t.Fatalf("run -d printed %q, expected a 64 character id", id)
	}

	ps, _ := mustRun(t, "ps")
	if !strings.Contains(ps, id[:12]) {
		t.Errorf("ps does not list the container:\n%s", ps)
	}
	logs, _ := mustRun(t, "logs", id)
	if !strings.Contains(logs, "started") {
		t.Errorf("logs = %q, want the container output", logs)
	}
	execOut, errOut := mustRun(t, "exec", id, "echo", "from-exec")
	if !strings.Contains(execOut, "from-exec") {
		t.Errorf("exec output = %q (%s)", execOut, errOut)
	}
	mustRun(t, "stop", "-t", "5", id)
	waitForExit(t, id, 30*time.Second)
	mustRun(t, "rm", id)
	if ids := containerIDs(t); len(ids) != 0 {
		t.Errorf("container not removed: %v", ids)
	}
}

// M5: exit codes propagate.
func TestM5ExitCodePropagation(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	_, _, code := mb(t, "run", "--rm", "--network", "none", testImage, "/bin/sh", "-c", "exit 42")
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
	_, _, code = mb(t, "run", "--rm", "--network", "none", testImage, "/bin/sh", "-c", "kill -TERM $$")
	if code != 143 {
		t.Errorf("signalled container exit code = %d, want 143", code)
	}
}

// dirSizeBytes sums the sizes of the files under a directory.
func dirSizeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// findShim returns the shim pid of a detached container.
func findShim(t *testing.T, containerID string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "minibox shim").Output()
	if err != nil {
		// Fall back to scanning /proc for the container id in the environment.
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
			if err != nil {
				continue
			}
			if strings.Contains(string(env), "MINIBOX_SHIM_ID="+containerID) {
				return pid
			}
		}
		return 0
	}
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			return pid
		}
	}
	return 0
}

// execKill9 sends SIGKILL to a pid.
func execKill9(pid int) error {
	return exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
}
