//go:build linux

package security

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Hosts routinely drop capabilities from the bounding set (Railway, Kubernetes
// restricted profiles and gVisor all do), so applying the Docker-like default
// set must degrade instead of failing. These tests run the real capset in a
// child process, because applying capabilities changes the calling thread.
const capsChildEnv = "MINIBOX_CAPS_TEST_CHILD"

// TestCapabilityChildHelper is the entry point of the child spawned below. It
// is a no-op in a normal run.
func TestCapabilityChildHelper(t *testing.T) {
	mode := os.Getenv(capsChildEnv)
	if mode == "" {
		t.Skip("helper entry point; spawned with " + capsChildEnv + " set")
	}
	var caps []int
	switch mode {
	case "default":
		var err error
		if caps, err = ResolveCapabilities(nil, nil); err != nil {
			fmt.Println("FAILED", err)
			os.Exit(3)
		}
	case "none":
		caps = nil
	case "all":
		var err error
		if caps, err = ResolveCapabilities([]string{"ALL"}, nil); err != nil {
			fmt.Println("FAILED", err)
			os.Exit(3)
		}
	default:
		fmt.Println("FAILED unknown mode", mode)
		os.Exit(3)
	}
	unavailable, err := ApplyCapabilities(caps)
	if err != nil {
		fmt.Println("FAILED", err)
		os.Exit(3)
	}
	fmt.Println("UNAVAILABLE", strings.Join(unavailable, ","))
	fmt.Println("OK")
	os.Exit(0)
}

// runCapsChild applies capabilities in a child process and returns its output.
func runCapsChild(t *testing.T, mode string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCapabilityChildHelper", "-test.v")
	cmd.Env = append(os.Environ(), capsChildEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestApplyCapabilitiesSurvivesRestrictedBoundingSet is the regression test for
// "apply capabilities: capset: operation not permitted" on hosts whose bounding
// set is smaller than the Docker-like default set.
func TestApplyCapabilitiesSurvivesRestrictedBoundingSet(t *testing.T) {
	want := defaultSetUnavailable(t)

	out, err := runCapsChild(t, "default")
	if err != nil {
		t.Fatalf("applying the default capability set failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") {
		t.Fatalf("child did not report success:\n%s", out)
	}
	got := parseUnavailable(out)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("unavailable capabilities = %v, want %v", got, want)
	}
	if len(want) > 0 {
		t.Logf("this host is missing %s; the container must still start", strings.Join(want, ", "))
	}
}

// TestApplyCapabilitiesEmptyAndAll covers the two extremes: dropping every
// capability (where the bounding-set drop can no longer be performed, because
// CAP_SETPCAP is gone) and requesting everything.
func TestApplyCapabilitiesEmptyAndAll(t *testing.T) {
	for _, mode := range []string{"none", "all"} {
		out, err := runCapsChild(t, mode)
		if err != nil {
			t.Errorf("mode %s failed: %v\n%s", mode, err, out)
			continue
		}
		if !strings.Contains(out, "OK") {
			t.Errorf("mode %s did not succeed:\n%s", mode, out)
		}
	}
}

// defaultSetUnavailable returns the default-set capabilities this host's
// bounding set does not provide.
func defaultSetUnavailable(t *testing.T) []string {
	t.Helper()
	caps, err := ResolveCapabilities(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, unavailable := SplitGrantable(caps)
	return unavailable
}

// parseUnavailable extracts the UNAVAILABLE line from child output.
func parseUnavailable(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "UNAVAILABLE") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "UNAVAILABLE"))
		if rest == "" {
			return nil
		}
		return strings.Split(rest, ",")
	}
	return nil
}
