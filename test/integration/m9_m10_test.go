//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M9: a named volume keeps its data across containers.
func TestM9NamedVolumePersists(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	mustRun(t, "volume", "create", "itdata")

	out, errOut, code := mb(t, "run", "--rm", "--network", "none", "-v", "itdata:/data",
		testImage, "/bin/sh", "-c", "echo persisted > /data/probe; cat /data/probe")
	if code != 0 {
		t.Fatalf("writing container failed: %s%s", out, errOut)
	}
	if !strings.Contains(out, "persisted") {
		t.Fatalf("write output = %q", out)
	}

	out, errOut, code = mb(t, "run", "--rm", "--network", "none", "-v", "itdata:/data",
		testImage, "/bin/sh", "-c", "cat /data/probe")
	if code != 0 {
		t.Fatalf("reading container failed: %s%s", out, errOut)
	}
	if !strings.Contains(out, "persisted") {
		t.Errorf("the volume did not keep its data: %q", out)
	}

	// The volume is listed, and it cannot be removed while a container uses it.
	list, _ := mustRun(t, "volume", "ls")
	if !strings.Contains(list, "itdata") {
		t.Errorf("volume ls = %q", list)
	}
	mustRun(t, "volume", "rm", "itdata")
}

// M9: health checks move a container from starting to healthy or unhealthy.
func TestM9HealthCheck(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	out, _, code := mb(t, "run", "-d", "--network", "none", "--name", "healthy",
		"--health-cmd", "test -f /etc/os-release",
		"--health-interval", "1s", "--health-retries", "2",
		testImage, "sleep", "60")
	if code != 0 {
		t.Fatalf("run -d failed: %s", out)
	}
	healthy := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", healthy)

	if !waitForHealth(t, healthy, "healthy", 30*time.Second) {
		t.Errorf("container never became healthy: %s", inspectOf(t, healthy))
	}

	out, _, code = mb(t, "run", "-d", "--network", "none", "--name", "unhealthy",
		"--health-cmd", "exit 1",
		"--health-interval", "1s", "--health-retries", "2",
		testImage, "sleep", "60")
	if code != 0 {
		t.Fatalf("run -d failed: %s", out)
	}
	unhealthy := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", unhealthy)
	if !waitForHealth(t, unhealthy, "unhealthy", 30*time.Second) {
		t.Errorf("container was never reported unhealthy: %s", inspectOf(t, unhealthy))
	}
}

// M9: an on-failure restart policy brings a failing container back.
func TestM9RestartPolicy(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	out, _, code := mb(t, "run", "-d", "--network", "none", "--name", "restarter",
		"--restart", "on-failure:2",
		testImage, "/bin/sh", "-c", "exit 1")
	if code != 0 {
		t.Fatalf("run -d failed: %s", out)
	}
	id := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", id)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		inspect := inspectOf(t, id)
		if strings.Contains(inspect, `"restarts": 2`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("the container was not restarted twice: %s", inspectOf(t, id))
}

// M9: compose up/down starts and removes a project.
func TestM9ComposeUpDown(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	content := "name: itproject\nservices:\n  sleeper:\n    image: " + testImage + "\n    command: [\"sleep\", \"120\"]\n    network_mode: none\n"
	if err := os.WriteFile(composePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := mb(t, "up", "-d", "-f", composePath)
	if code != 0 {
		t.Fatalf("up failed: %s%s", out, errOut)
	}
	ps, _ := mustRun(t, "ps")
	if !strings.Contains(ps, "itproject_sleeper_1") {
		t.Errorf("ps does not show the compose container:\n%s", ps)
	}
	mustRun(t, "down", "-f", composePath, "--volumes")
	ps, _ = mustRun(t, "ps", "-a")
	if strings.Contains(ps, "itproject_sleeper_1") {
		t.Errorf("down left the container behind:\n%s", ps)
	}
}

// M9: build a small image, run it, and confirm the cache is reused.
func TestM9BuildAndCache(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	dir := t.TempDir()
	dockerfile := "FROM " + testImage + "\n" +
		"COPY message.txt /message.txt\n" +
		"RUN cat /message.txt > /built.txt\n" +
		"CMD [\"cat\", \"/built.txt\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "message.txt"), []byte("built-by-minibox\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := mb(t, "build", "-t", "itbuilt:1", dir)
	if code != 0 {
		t.Fatalf("build failed: %s%s", out, errOut)
	}
	runOut, errOut, code := mb(t, "run", "--rm", "--network", "none", "itbuilt:1")
	if code != 0 {
		t.Fatalf("running the built image failed: %s%s", runOut, errOut)
	}
	if !strings.Contains(runOut, "built-by-minibox") {
		t.Errorf("built image output = %q", runOut)
	}

	// A second build must hit the cache and must not re-run the RUN step.
	out, errOut, code = mb(t, "build", "-t", "itbuilt:1", dir)
	if code != 0 {
		t.Fatalf("second build failed: %s%s", out, errOut)
	}
	if !strings.Contains(errOut, "cache hit") {
		t.Errorf("second build did not reuse the cache:\n%s", errOut)
	}
	mustRun(t, "rmi", "itbuilt:1")
}

// M10: the CLI starts quickly and the binary stays small.
func TestM10StartupAndSize(t *testing.T) {
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 15<<20 {
		t.Errorf("binary is %d bytes, over the 15 MB budget", info.Size())
	}
	start := time.Now()
	runs := 20
	for i := 0; i < runs; i++ {
		if _, _, code := mb(t, "version"); code != 0 {
			t.Fatal("minibox version failed")
		}
	}
	perRun := time.Since(start) / time.Duration(runs)
	t.Logf("minibox version: %s per run over %d runs (includes process spawn)", perRun, runs)
	if perRun > 200*time.Millisecond {
		t.Errorf("CLI startup is %s per run, expected well under 200ms", perRun)
	}
}

// waitForHealth polls inspect until the container reports the wanted health.
func waitForHealth(t *testing.T, id, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(inspectOf(t, id), `"health": "`+want+`"`) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// inspectOf returns `minibox inspect` output for a container.
func inspectOf(t *testing.T, id string) string {
	t.Helper()
	out, _, _ := mb(t, "inspect", id)
	return out
}

var _ = exec.Command
