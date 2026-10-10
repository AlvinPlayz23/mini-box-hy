//go:build linux

package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// seccompEmulated reports whether this kernel is an emulator such as gVisor,
// where SECCOMP_MODE_FILTER is emulated and installing a filter can block
// outright. The live tests skip there; the interpreter tests still run and are
// the ones that catch decision bugs, and real hosts run both.
func seccompEmulated() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "gvisor")
}

// The live tests really install a filter in a child process: that is the only
// way to catch a filter that is well-formed but decides the wrong thing (an
// inverted jump, for example, kills the process on its first syscall).
const (
	seccompChildEnv  = "MINIBOX_SECCOMP_TEST_CHILD"
	childDefaultMode = "default"
	childKillMode    = "foreign-arch"
)

// TestSeccompChildHelper is the entry point of the child processes spawned by
// the tests below. It is a no-op in a normal run.
func TestSeccompChildHelper(t *testing.T) {
	mode := os.Getenv(seccompChildEnv)
	if mode == "" {
		t.Skip("helper entry point; spawned with " + seccompChildEnv + " set")
	}
	var filter []unix.SockFilter
	switch mode {
	case childDefaultMode:
		f, err := BuildFilter(DefaultProfile())
		if err != nil {
			fmt.Println("BUILD-FAILED", err)
			os.Exit(4)
		}
		filter = f
	case childKillMode:
		// A filter whose architecture guard can never match: every syscall
		// after installation must be killed, which proves the kill path works
		// and that this harness detects it.
		filter = []unix.SockFilter{
			{Code: bpfLD | bpfW | bpfABS, K: seccompDataArch},
			{Code: bpfJMP | bpfJEQ | bpfK, Jt: 1, Jf: 0, K: 0xdeadbeef},
			{Code: bpfRET | bpfK, K: seccompRetKillProcess},
			{Code: bpfRET | bpfK, K: seccompRetAllow},
		}
	default:
		fmt.Println("UNKNOWN-MODE")
		os.Exit(5)
	}
	if err := ApplySeccomp(filter); err != nil {
		fmt.Println("SECCOMP-UNSUPPORTED", err)
		os.Exit(3)
	}
	// The marker write is the first syscall after the filter is in force.
	fmt.Println("SURVIVED", mode)
	os.Exit(0)
}

// runSeccompChild runs the helper in the given mode and returns its output.
func runSeccompChild(t *testing.T, mode string) (string, *exec.ExitError) {
	t.Helper()
	// A seccomp harness must never hang the suite: the child gets a hard
	// deadline, and hitting it is reported rather than waited on.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSeccompChildHelper", "-test.v")
	cmd.Env = append(os.Environ(), seccompChildEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("seccomp child (%s) did not finish within 30s; output so far:\n%s", mode, out)
	}
	if err == nil {
		return string(out), nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run child: %v", err)
	}
	return string(out), exitErr
}

// TestDefaultFilterLetsNativeProcessLive installs the real default profile in a
// child and requires it to keep running: the field failure this guards against
// is the container dying with EOF during setup.
func TestDefaultFilterLetsNativeProcessLive(t *testing.T) {
	if seccompEmulated() {
		t.Skip("seccomp filters are emulated on this kernel (gVisor); run this test on a real Linux host")
	}
	out, exitErr := runSeccompChild(t, childDefaultMode)
	if strings.Contains(out, "SECCOMP-UNSUPPORTED") {
		t.Skipf("seccomp filters are unavailable here: %s", strings.TrimSpace(out))
	}
	if exitErr != nil {
		t.Fatalf("a process with the default profile did not survive its first syscall: %v\n%s", exitErr, out)
	}
	if !strings.Contains(out, "SURVIVED") {
		t.Fatalf("child did not report survival:\n%s", out)
	}
}

// TestForeignArchFilterKillsProcess proves the kill action is reachable and
// detected, so the survival test above cannot pass vacuously.
func TestForeignArchFilterKillsProcess(t *testing.T) {
	if seccompEmulated() {
		t.Skip("seccomp filters are emulated on this kernel (gVisor); run this test on a real Linux host")
	}
	out, exitErr := runSeccompChild(t, childKillMode)
	if strings.Contains(out, "SECCOMP-UNSUPPORTED") {
		t.Skipf("seccomp filters are unavailable here: %s", strings.TrimSpace(out))
	}
	if exitErr == nil {
		t.Fatalf("a process with a foreign-architecture filter survived:\n%s", out)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("unexpected exit error: %v", exitErr)
	}
	if !status.Signaled() || status.Signal() != syscall.SIGSYS {
		t.Fatalf("child ended with %v, want death by SIGSYS", status)
	}
}
