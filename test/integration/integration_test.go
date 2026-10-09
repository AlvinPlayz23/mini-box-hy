//go:build integration

// Package integration holds the acceptance tests from PLAN.md. They need a real
// Linux host (or VM) that can create namespaces, mount overlayfs and use cgroup
// v2. Run them with scripts/run-integration.sh.
package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	binPath   string
	rootDir   string
	testImage string
)

func TestMain(m *testing.M) {
	binPath = os.Getenv("MINIBOX_BIN")
	if binPath == "" {
		dir, err := os.MkdirTemp("", "minibox-bin-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		binPath = filepath.Join(dir, "minibox")
		cmd := exec.Command("go", "build", "-trimpath", "-o", binPath, "./cmd/minibox")
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build minibox: %v\n%s\n", err, out)
			os.Exit(1)
		}
	}
	rootDir = os.Getenv("MINIBOX_ROOT")
	if rootDir == "" {
		dir, err := os.MkdirTemp("", "minibox-it-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		rootDir = dir
	}
	testImage = os.Getenv("MINIBOX_TEST_IMAGE")
	if testImage == "" {
		testImage = "alpine:3.20"
	}
	code := m.Run()
	os.RemoveAll(rootDir)
	os.Exit(code)
}

// mb runs the minibox binary against the test state root.
func mb(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return mbStdin(t, nil, args...)
}

func mbStdin(t *testing.T, stdin *bytes.Reader, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "MINIBOX_ROOT="+rootDir)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err := cmd.Run()
	code = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run minibox %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// mustRun fails the test when the command exits non-zero.
func mustRun(t *testing.T, args ...string) (string, string) {
	t.Helper()
	out, errOut, code := mb(t, args...)
	if code != 0 {
		t.Fatalf("minibox %v exited %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out, errOut
}

// ensureImage pulls the test image once per suite run.
func ensureImage(t *testing.T) {
	t.Helper()
	if _, _, code := mb(t, "images"); code != 0 {
		t.Fatal("minibox images failed")
	}
	if _, errOut, code := mb(t, "pull", testImage); code != 0 {
		t.Skipf("cannot pull %s (no registry access?): %s", testImage, errOut)
	}
}

// rootfsFor returns an unpacked rootfs directory of the test image.
func rootfsFor(t *testing.T) string {
	t.Helper()
	ensureImage(t)
	matches, err := filepath.Glob(filepath.Join(rootDir, "layers", "*", "diff"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no unpacked layer under %s: %v", rootDir, err)
	}
	return matches[0]
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

func requireCgroupV2(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("needs cgroup v2")
	}
}

func requireNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv("MINIBOX_SKIP_NETWORK_TESTS") != "" {
		t.Skip("network tests disabled")
	}
}

// leakedMounts returns mount points under the test state root.
func leakedMounts(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if strings.HasPrefix(fields[4], rootDir) {
			out = append(out, fields[4])
		}
	}
	return out
}

// containerIDs lists container ids in the store.
func containerIDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(rootDir, "containers"))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids
}

// waitForExit polls until a container reports a finished state.
func waitForExit(t *testing.T, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _, code := mb(t, "ps", "-a", "--json")
		if code == 0 && strings.Contains(out, id) && strings.Contains(out, `"status": "exited"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s did not exit within %s", id[:12], timeout)
}
