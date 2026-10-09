//go:build linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/volume"
)

func TestNamedVolumeMountResolution(t *testing.T) {
	env, _, _ := testEnv(t)
	archive := writeRootfsArchive(t)
	run(t, env, "load", "-t", "volimg:1", archive)

	cfgJSON := run(t, env, "run", "--dry-run", "--network", "none",
		"-v", "appdata:/var/lib/appdata",
		"-v", "cache:/cache:ro",
		"volimg:1", "true")
	var cfg runtime.Config
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mounts) != 2 {
		t.Fatalf("mounts = %+v, want two", cfg.Mounts)
	}
	store, err := volumeStore(env)
	if err != nil {
		t.Fatal(err)
	}
	v, err := store.Get("appdata")
	if err != nil {
		t.Fatalf("named volume was not created: %v", err)
	}
	if cfg.Mounts[0].Source != v.Mountpoint || cfg.Mounts[0].Destination != "/var/lib/appdata" {
		t.Errorf("mount = %+v, want source %s", cfg.Mounts[0], v.Mountpoint)
	}
	if !cfg.Mounts[1].ReadOnly {
		t.Errorf("second mount should be read-only: %+v", cfg.Mounts[1])
	}

	// A host path must still be treated as a path, not a volume name.
	hostDir := t.TempDir()
	cfgJSON = run(t, env, "run", "--dry-run", "--network", "none", "-v", hostDir+":/host", "volimg:1", "true")
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Mounts[0].Source != hostDir {
		t.Errorf("host mount source = %q, want %q", cfg.Mounts[0].Source, hostDir)
	}
}

func TestVolumeCommands(t *testing.T) {
	env, out, _ := testEnv(t)
	run(t, env, "volume", "create", "data")
	if !strings.Contains(out.String(), "data") {
		t.Errorf("create output = %q", out.String())
	}
	run(t, env, "volume", "create", "logs")
	list := run(t, env, "volume", "ls", "--json")
	var volumes []volume.Volume
	if err := json.Unmarshal([]byte(list), &volumes); err != nil {
		t.Fatalf("volume ls --json: %v", err)
	}
	if len(volumes) != 2 {
		t.Fatalf("volumes = %+v, want two", volumes)
	}
	run(t, env, "volume", "rm", "logs")
	remaining := run(t, env, "volume", "ls")
	if strings.Contains(remaining, "logs") {
		t.Errorf("removed volume still listed: %s", remaining)
	}
	if code := RunWithEnv(env, []string{"volume", "rm", "missing"}); code == 0 {
		t.Error("removing a missing volume succeeded")
	}
}

func TestHealthAndRestartFlagValidation(t *testing.T) {
	env, _, errOut := testEnv(t)
	archive := writeRootfsArchive(t)
	run(t, env, "load", "-t", "himg:1", archive)

	cfgJSON := run(t, env, "run", "--dry-run", "--network", "none",
		"--health-cmd", "test -f /tmp/ok", "--health-interval", "5s", "--health-retries", "4",
		"himg:1", "true")
	var cfg runtime.Config
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HealthCheck == nil {
		t.Fatal("health check missing from the resolved config")
	}
	if cfg.HealthCheck.Interval.String() != "5s" || cfg.HealthCheck.Retries != 4 {
		t.Errorf("health check = %+v", cfg.HealthCheck)
	}
	if cfg.HealthCheck.Test[0] != "CMD-SHELL" {
		t.Errorf("health test = %v", cfg.HealthCheck.Test)
	}

	// Restart policies need -d.
	errOut.Reset()
	if code := RunWithEnv(env, []string{"run", "--dry-run", "--restart", "always", "himg:1", "true"}); code == 0 {
		t.Error("foreground --restart was accepted")
	}
	if !strings.Contains(errOut.String(), "needs -d") {
		t.Errorf("error does not explain the -d requirement: %s", errOut.String())
	}
	cfgJSON = run(t, env, "run", "--dry-run", "-d", "--restart", "on-failure:2", "--network", "none", "himg:1", "true")
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Restart != "on-failure:2" {
		t.Errorf("restart = %q", cfg.Restart)
	}

	// Health flags without --health-cmd are a usage error.
	errOut.Reset()
	if code := RunWithEnv(env, []string{"run", "--dry-run", "--health-interval", "5s", "himg:1", "true"}); code == 0 {
		t.Error("--health-interval without --health-cmd was accepted")
	}
	// Invalid durations and policies are rejected.
	for _, args := range [][]string{
		{"run", "--dry-run", "--health-cmd", "true", "--health-interval", "soon", "himg:1", "true"},
		{"run", "--dry-run", "-d", "--restart", "sometimes", "himg:1", "true"},
		{"run", "--dry-run", "--memory", "1gig", "himg:1", "true"},
	} {
		if code := RunWithEnv(env, args); code == 0 {
			t.Errorf("%v was accepted, want an error", args)
		}
	}
}

func TestComposeNamingAndSanitising(t *testing.T) {
	if got := projectContainerName("My App", "Web Server"); got != "my_app_web_server_1" {
		t.Errorf("projectContainerName = %q", got)
	}
	if got := sanitizeName(""); got != "default" {
		t.Errorf("sanitizeName(\"\") = %q", got)
	}
	if got := tagName("ghcr.io/owner/app:1.2"); got != "app" {
		t.Errorf("tagName = %q", got)
	}
	if got := tagPart("ghcr.io/owner/app:1.2"); got != "1.2" {
		t.Errorf("tagPart = %q", got)
	}
	if got := tagPart("app"); got != "latest" {
		t.Errorf("tagPart(no tag) = %q", got)
	}
}

func TestBuildHelpers(t *testing.T) {
	if got := strings.Join(splitFields(`a "b c" 'd e'`), "|"); got != "a|b c|d e" {
		t.Errorf("splitFields = %q", got)
	}
	kv := parseKeyValues(`FOO=bar BAZ="qu ux"`)
	if kv["FOO"] != "bar" || kv["BAZ"] != "qu ux" {
		t.Errorf("parseKeyValues = %v", kv)
	}
	kv = parseKeyValues("KEY value")
	if kv["KEY"] != "value" {
		t.Errorf("parseKeyValues(space form) = %v", kv)
	}
}

func TestDownWithNoContainers(t *testing.T) {
	env, out, _ := testEnv(t)
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("name: demo\nservices:\n  web:\n    image: alpine:3.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, "down", "-f", composePath)
	if !strings.Contains(out.String(), "no containers") {
		t.Errorf("down output = %q", out.String())
	}
}
