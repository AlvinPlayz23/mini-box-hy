//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/api"
	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// imageConfigForTest is a small image configuration for the merge tests.
func imageConfigForTest() image.ImageConfig {
	return image.ImageConfig{
		Entrypoint: []string{"/entry", "--flag"},
		Cmd:        []string{"/img-cmd"},
	}
}

// imageConfigEmpty is an image with neither entrypoint nor cmd.
func imageConfigEmpty() image.ImageConfig { return image.ImageConfig{} }

func TestNetworkModeMapping(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "none", want: "none"},
		{in: "host", want: "host"},
		{in: "bridge", want: "bridge"},
		{in: "pasta", want: "pasta"},
		{in: "default", want: "bridge"},
		{in: "", want: "bridge"},
		{in: "container:abc", wantErr: true},
	}
	for _, tt := range tests {
		got, err := networkMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("networkMode(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("networkMode(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("networkMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseSignal(t *testing.T) {
	tests := []struct {
		in      string
		want    syscall.Signal
		wantErr bool
	}{
		{in: "", want: syscall.SIGTERM},
		{in: "TERM", want: syscall.SIGTERM},
		{in: "SIGTERM", want: syscall.SIGTERM},
		{in: "sigkill", want: syscall.SIGKILL},
		{in: "9", want: syscall.SIGKILL},
		{in: "HUP", want: syscall.SIGHUP},
		{in: "WINCH", want: syscall.SIGWINCH},
		{in: "0", wantErr: true},
		{in: "999", wantErr: true},
		{in: "NOTASIGNAL", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseSignal(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSignal(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSignal(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSignal(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRestartString(t *testing.T) {
	tests := []struct {
		in      api.RestartPolicy
		want    string
		wantErr bool
	}{
		{in: api.RestartPolicy{}, want: "no"},
		{in: api.RestartPolicy{Name: "no"}, want: "no"},
		{in: api.RestartPolicy{Name: "always"}, want: "always"},
		{in: api.RestartPolicy{Name: "unless-stopped"}, want: "unless-stopped"},
		{in: api.RestartPolicy{Name: "on-failure"}, want: "on-failure"},
		{in: api.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}, want: "on-failure:3"},
		{in: api.RestartPolicy{Name: "sometimes"}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := restartString(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("restartString(%+v) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("restartString(%+v): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("restartString(%+v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApplyPortBindings(t *testing.T) {
	cfg := &runtime.Config{}
	bindings := map[string][]api.PortBinding{
		"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8080"}},
		"53/udp": {{HostPort: "5353"}},
		"9000":   {{HostPort: ""}},
	}
	if err := applyPortBindings(cfg, bindings); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Network.Ports) != 3 {
		t.Fatalf("ports = %+v, want three", cfg.Network.Ports)
	}
	found := map[int]runtime.PortMapping{}
	for _, p := range cfg.Network.Ports {
		found[p.ContainerPort] = p
	}
	if p := found[80]; p.HostPort != 8080 || p.HostIP != "127.0.0.1" || p.Protocol != "tcp" {
		t.Errorf("80/tcp mapped to %+v", p)
	}
	if p := found[53]; p.HostPort != 5353 || p.Protocol != "udp" {
		t.Errorf("53/udp mapped to %+v", p)
	}
	if p := found[9000]; p.HostPort != 0 || p.Protocol != "tcp" {
		t.Errorf("9000 mapped to %+v", p)
	}
	if err := applyPortBindings(&runtime.Config{}, map[string][]api.PortBinding{"nope": nil}); err == nil {
		t.Error("an invalid port key was accepted")
	}
}

func TestMergeCommandAndEnv(t *testing.T) {
	img := imageConfigForTest()
	argv, err := mergeCommand(img, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "/entry --flag /img-cmd" {
		t.Errorf("image entrypoint+cmd = %v", argv)
	}
	argv, err = mergeCommand(img, []string{"/new"}, []string{"sub"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "/new sub" {
		t.Errorf("override = %v", argv)
	}
	argv, err = mergeCommand(img, nil, []string{"only"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "/entry --flag only" {
		t.Errorf("cmd override = %v", argv)
	}
	if _, err := mergeCommand(imageConfigEmpty(), nil, nil); err == nil {
		t.Error("an empty image command was accepted")
	}

	env := mergeEnv([]string{"A=1", "PATH=/img/bin"}, []string{"A=2", "B=3"})
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "A=2") || !strings.Contains(joined, "B=3") || !strings.Contains(joined, "PATH=/img/bin") {
		t.Errorf("env = %v", env)
	}
	if strings.Contains(joined, "A=1") {
		t.Errorf("override did not replace the image value: %v", env)
	}
	env = mergeEnv(nil, nil)
	if !strings.Contains(strings.Join(env, " "), "PATH=") {
		t.Errorf("default PATH missing: %v", env)
	}
}

func TestSkipLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	content := "one\ntwo\nthree\nfour\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	skipLines(f, 2)
	rest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = rest
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	got := string(buf[:n])
	if got != "three\nfour\n" {
		t.Errorf("tail of 2 lines = %q", got)
	}
}

func TestInspectAndListShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv(container.RootEnv, root)
	store, err := container.OpenAt(root)
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{
		Store:   store,
		labels:  map[string]map[string]string{},
		running: map[string]*container.Started{},
		starts:  map[string]*sync.Mutex{},
	}
	cfg := &runtime.Config{
		Image:    "docker.io/library/alpine:3.20",
		Args:     []string{"sleep", "10"},
		Env:      []string{"PATH=/bin"},
		Cwd:      "/",
		Hostname: "box",
		Rootfs:   runtime.Rootfs{Lower: []string{"/tmp/none"}, Overlay: true},
		Network:  runtime.Network{Mode: "none", Ports: []runtime.PortMapping{{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}},
		Created:  time.Now().UTC(),
	}
	c, err := container.Create(store, cfg, "webtest")
	if err != nil {
		t.Fatal(err)
	}
	c.State.Status = container.StatusExited
	c.State.ExitCode = 3
	c.State.Health = "healthy"
	if err := c.SaveState(); err != nil {
		t.Fatal(err)
	}

	list, err := b.ListContainers(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list[0].ID != c.ID || list[0].Names[0] != "/webtest" || list[0].State != "exited" {
		t.Errorf("summary = %+v", list[0])
	}
	if len(list[0].Ports) != 1 || list[0].Ports[0].PrivatePort != 80 || list[0].Ports[0].PublicPort != 8080 {
		t.Errorf("ports = %+v", list[0].Ports)
	}
	if !strings.Contains(list[0].Status, "Exited (3)") {
		t.Errorf("status = %q", list[0].Status)
	}
	if list[0].Health != "healthy" {
		t.Errorf("health = %q", list[0].Health)
	}

	raw, err := b.InspectContainer(nil, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("inspect JSON: %v", err)
	}
	for _, key := range []string{"Id", "Name", "Created", "Image", "State", "Config", "Mounts", "NetworkSettings", "HostConfig"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("inspect JSON lacks %s: %s", key, raw)
		}
	}
	state, ok := parsed["State"].(map[string]any)
	if !ok || state["Running"] != false || state["ExitCode"] != float64(3) {
		t.Errorf("inspect State = %v", parsed["State"])
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := b.WaitContainer(waitCtx, c.ID)
	if err != nil || code != 3 {
		t.Errorf("WaitContainer = %d, %v; want 3", code, err)
	}
}
