//go:build linux

package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// testEnv builds a CLI environment whose output is captured and whose state
// root is a temp dir, as the working agreements require.
func testEnv(t *testing.T) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("MINIBOX_ROOT", t.TempDir())
	env, err := NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	env.Stdout, env.Stderr = out, errOut
	env.Progress = func(msg string) { errOut.WriteString("minibox: " + msg + "\n") }
	return env, out, errOut
}

// run invokes the CLI and fails the test on a non-zero exit code.
func run(t *testing.T, env *Env, args ...string) string {
	t.Helper()
	out, errOut := env.Stdout.(*bytes.Buffer), env.Stderr.(*bytes.Buffer)
	out.Reset()
	errOut.Reset()
	if code := RunWithEnv(env, args); code != 0 {
		t.Fatalf("minibox %v exited %d: %s", args, code, errOut.String())
	}
	return out.String()
}

// writeRootfsArchive creates a small gzipped rootfs tarball.
func writeRootfsArchive(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "testrootfs.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name string
		body string
		mode int64
	}{
		{"etc/passwd", "root:x:0:0:root:/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n", 0o644},
		{"bin/sh", "#!/bin/sh\n", 0o755},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLILoadImagesAndDryRun(t *testing.T) {
	env, _, _ := testEnv(t)
	archive := writeRootfsArchive(t)

	if out := run(t, env, "load", "-t", "cliimg:1", archive); !strings.Contains(out, "cliimg:1") {
		t.Fatalf("load output = %q", out)
	}

	// images --json must describe the imported image.
	var records []map[string]any
	if err := json.Unmarshal([]byte(run(t, env, "images", "--json")), &records); err != nil {
		t.Fatalf("images --json: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("images --json returned %d records, want 1", len(records))
	}
	if records[0]["ref"] != "docker.io/library/cliimg:1" {
		t.Errorf("ref = %v", records[0]["ref"])
	}

	// run --dry-run resolves flags, image config and defaults.
	cfgJSON := run(t, env, "run", "--dry-run",
		"-e", "FOO=bar",
		"--memory", "50m", "--cpus", "0.5", "--pids-limit", "100",
		"--cap-drop", "ALL", "--cap-add", "SYS_ADMIN",
		"--network", "none", "--hostname", "drybox", "--read-only", "--init",
		"cliimg:1", "echo", "hi")
	var cfg runtime.Config
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatalf("parse dry-run output: %v\n%s", err, cfgJSON)
	}
	if len(cfg.Args) != 2 || cfg.Args[0] != "echo" || cfg.Args[1] != "hi" {
		t.Errorf("args = %v, want [echo hi]", cfg.Args)
	}
	if cfg.Hostname != "drybox" || cfg.Network.Mode != "none" {
		t.Errorf("hostname/network = %q/%q", cfg.Hostname, cfg.Network.Mode)
	}
	if !cfg.ReadOnly || !cfg.Init {
		t.Error("--read-only/--init were not recorded")
	}
	if cfg.Resources.MemoryBytes != 50<<20 || cfg.Resources.CPUs != "0.5" || cfg.Resources.PidsLimit != 100 {
		t.Errorf("resources = %+v", cfg.Resources)
	}
	if !containsString(cfg.Env, "FOO=bar") {
		t.Errorf("env = %v, want FOO=bar", cfg.Env)
	}
	if !containsString(cfg.Env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Errorf("env = %v, want a default PATH", cfg.Env)
	}
	if len(cfg.Security.Capabilities) != 1 || cfg.Security.Capabilities[0] != "CAP_SYS_ADMIN" {
		t.Errorf("capabilities = %v, want only CAP_SYS_ADMIN", cfg.Security.Capabilities)
	}
	if cfg.Security.Seccomp != "default" || !cfg.Security.NoNewPrivs {
		t.Errorf("security = %+v", cfg.Security)
	}
	if len(cfg.Rootfs.Lower) != 1 || !cfg.Rootfs.Overlay {
		t.Errorf("rootfs = %+v", cfg.Rootfs)
	}

	// The image's /etc/passwd must resolve --user names.
	cfgJSON = run(t, env, "run", "--dry-run", "-u", "app", "cliimg:1", "id")
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.User != "app" {
		t.Errorf("user = %q, want app", cfg.User)
	}

	// ps --json must be a well-formed empty list before any run.
	var states []map[string]any
	if err := json.Unmarshal([]byte(run(t, env, "ps", "-a", "--json")), &states); err != nil {
		t.Fatalf("ps --json: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("ps listed %d containers, want 0", len(states))
	}

	// rmi removes the record and its layer chain.
	run(t, env, "rmi", "cliimg:1")
	if out := run(t, env, "images"); strings.Contains(out, "cliimg") {
		t.Errorf("image still listed after rmi: %s", out)
	}
	layers, err := os.ReadDir(env.Store.LayersDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 0 {
		t.Errorf("layer chains left behind: %v", layers)
	}
}

func TestCLIErrorsAreActionable(t *testing.T) {
	env, _, _ := testEnv(t)
	env.Stderr.(*bytes.Buffer).Reset()

	code := RunWithEnv(env, []string{"run", "--dry-run", "missing-image:1", "true"})
	if code == 0 {
		t.Fatal("running a missing image succeeded")
	}
	errText := env.Stderr.(*bytes.Buffer).String()
	if !strings.Contains(errText, "pull") {
		t.Errorf("error does not suggest pulling: %s", errText)
	}

	env.Stderr.(*bytes.Buffer).Reset()
	if code := RunWithEnv(env, []string{"run", "--network", "bogus", "missing-image:1", "true"}); code == 0 {
		t.Error("an invalid network mode was accepted")
	}
	if !strings.Contains(env.Stderr.(*bytes.Buffer).String(), "network mode") {
		t.Errorf("error does not mention the network mode: %s", env.Stderr.(*bytes.Buffer).String())
	}

	env.Stderr.(*bytes.Buffer).Reset()
	if code := RunWithEnv(env, []string{"stop", "nosuchcontainer"}); code == 0 {
		t.Error("stopping an unknown container succeeded")
	}
	if !strings.Contains(env.Stderr.(*bytes.Buffer).String(), "ps -a") {
		t.Errorf("error does not point at `ps -a`: %s", env.Stderr.(*bytes.Buffer).String())
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
