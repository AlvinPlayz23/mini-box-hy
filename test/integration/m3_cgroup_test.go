//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M3: exceeding --memory kills the container and the reason is reported.
func TestM3MemoryLimitOOM(t *testing.T) {
	requireRoot(t)
	requireCgroupV2(t)
	ensureImage(t)
	// /dev/shm is tmpfs, so the writes are charged to the container's cgroup.
	out, errOut, code := mb(t, "run", "--rm", "--network", "none", "--memory", "50m",
		testImage, "/bin/sh", "-c", "dd if=/dev/zero of=/dev/shm/fill bs=1M count=200 2>/dev/null; echo survived")
	if code == 0 && strings.Contains(out, "survived") {
		t.Fatalf("a 200MB write under a 50m limit survived: %s%s", out, errOut)
	}
	if code != 137 && code != 1 {
		t.Logf("exit code was %d (137 = SIGKILL from the OOM killer)", code)
	}

	// The OOM flag must be recorded on a container that is kept around.
	out, _, _ = mb(t, "run", "-d", "--network", "none", "--memory", "50m", "--name", "oomtest",
		testImage, "/bin/sh", "-c", "dd if=/dev/zero of=/dev/shm/fill bs=1M count=200 2>/dev/null; sleep 30")
	id := strings.TrimSpace(out)
	if id == "" {
		t.Fatalf("no container id from run -d: %s", out)
	}
	waitForExit(t, id, 60*time.Second)
	inspect, _, _ := mb(t, "inspect", id)
	if !strings.Contains(inspect, `"oom_killed": true`) {
		t.Errorf("inspect does not report oom_killed:\n%s", inspect)
	}
	mb(t, "rm", "-f", id)
}

// M3: --pids-limit stops a fork bomb.
func TestM3PidsLimit(t *testing.T) {
	requireRoot(t)
	requireCgroupV2(t)
	ensureImage(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mb(t, "run", "--rm", "--network", "none", "--pids-limit", "20",
			testImage, "/bin/sh", "-c", "i=0; while [ $i -lt 200 ]; do sleep 5 & i=$((i+1)); done; echo spawned_all")
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the container with --pids-limit never finished (fork bomb not contained)")
	}
}

// M3: cgroups are cleaned up after a normal exit and after killing the
// supervisor.
func TestM3CgroupCleanup(t *testing.T) {
	requireRoot(t)
	requireCgroupV2(t)
	ensureImage(t)
	cgroupParent := cgroupDirFor(t)

	out, errOut, code := mb(t, "run", "--rm", "--network", "none", testImage, "/bin/true")
	if code != 0 {
		t.Fatalf("run failed: %s%s", out, errOut)
	}
	if entries, err := os.ReadDir(cgroupParent); err == nil {
		for _, e := range entries {
			if isHexID(e.Name()) {
				t.Errorf("cgroup %s survived a normal exit", e.Name())
			}
		}
	}

	// Kill the shim and check that prune cleans up.
	out, _, code = mb(t, "run", "-d", "--network", "none", "--name", "killtest", testImage, "sleep", "300")
	if code != 0 {
		t.Fatalf("run -d failed: %s", out)
	}
	id := strings.TrimSpace(out)
	shimPID := findShim(t, id)
	if shimPID > 0 {
		if err := execKill9(shimPID); err != nil {
			t.Fatalf("kill -9 the shim: %v", err)
		}
	}
	mustRun(t, "system", "prune")
	if entries, err := os.ReadDir(cgroupParent); err == nil {
		for _, e := range entries {
			if isHexID(e.Name()) {
				t.Errorf("cgroup %s survived prune after the supervisor was killed", e.Name())
			}
		}
	}
	if mounts := leakedMounts(t); len(mounts) > 0 {
		t.Errorf("leaked mounts: %v", mounts)
	}
}

// M5: 100 sequential runs leave nothing behind.
func TestM5HundredRunsLeaveNothing(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	for i := 0; i < 100; i++ {
		if _, errOut, code := mb(t, "run", "--rm", "--network", "none", testImage, "/bin/true"); code != 0 {
			t.Fatalf("run %d failed: %s", i, errOut)
		}
	}
	if ids := containerIDs(t); len(ids) > 0 {
		t.Errorf("%d containers left behind: %v", len(ids), ids)
	}
	if mounts := leakedMounts(t); len(mounts) > 0 {
		t.Errorf("leaked mounts: %v", mounts)
	}
}

// cgroupDirFor returns the minibox cgroup directory of the current process.
func cgroupDirFor(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("cannot read /proc/self/cgroup")
	}
	line := strings.TrimSpace(strings.Split(string(data), "\n")[0])
	parts := strings.SplitN(line, ":", 3)
	if len(parts) != 3 {
		t.Skipf("unexpected /proc/self/cgroup content %q", line)
	}
	return filepath.Join("/sys/fs/cgroup", parts[2], "minibox")
}

func isHexID(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
