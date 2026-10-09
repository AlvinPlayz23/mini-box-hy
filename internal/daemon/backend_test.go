//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
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

// newTestBackend returns a backend over an empty temp state root.
func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedRecord stores a minimal image record so create tests need no network.
func seedImage(t *testing.T, b *Backend) *image.Record {
	t.Helper()
	rec := &image.Record{
		Ref:        "docker.io/library/alpine:3.20",
		Registry:   "docker.io",
		Repository: "library/alpine",
		Tag:        "3.20",
		Digest:     "sha256:abc123",
		ChainID:    "chain123",
		Size:       1024,
		Created:    time.Now().UTC(),
		Config: image.ConfigFile{
			Config: image.ImageConfig{
				Env:        []string{"PATH=/usr/bin"},
				Entrypoint: []string{"/entry", "--flag"},
				Cmd:        []string{"/img-cmd"},
				User:       "1000",
				WorkingDir: "/srv",
			},
		},
	}
	if err := b.Images.SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCreateContainerMapsRequest(t *testing.T) {
	b := newTestBackend(t)
	seedImage(t, b)
	src := t.TempDir()
	limit := int64(100)
	resp, err := b.CreateContainer(context.Background(), api.CreateRequest{
		Image:      "alpine:3.20",
		Cmd:        []string{"echo", "hi"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/work",
		User:       "root",
		Tty:        true,
		OpenStdin:  true,
		Labels:     map[string]string{"team": "core"},
		HostConfig: api.HostConfig{
			Binds:          []string{src + ":/data:ro"},
			Memory:         4096,
			NanoCPUs:       500000000,
			PidsLimit:      &limit,
			AutoRemove:     true,
			ReadonlyRootfs: true,
			NetworkMode:    "none",
			CapAdd:         []string{"NET_ADMIN"},
			CapDrop:        []string{"CHOWN"},
			Tmpfs:          map[string]string{"/tmp": ""},
			RestartPolicy:  api.RestartPolicy{Name: "on-failure", MaximumRetryCount: 2},
			PortBindings:   map[string][]api.PortBinding{"80/tcp": {{HostPort: "8080"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := runtime.LoadConfig(filepath.Join(b.Store.Dir(resp.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"/entry", "--flag", "echo", "hi"}
	if !reflect.DeepEqual(cfg.Args, wantArgs) {
		t.Errorf("args = %v, want %v", cfg.Args, wantArgs)
	}
	if !slices.Contains(cfg.Env, "FOO=bar") || !slices.Contains(cfg.Env, "PATH=/usr/bin") {
		t.Errorf("env = %v", cfg.Env)
	}
	if cfg.Cwd != "/work" || cfg.User != "root" || !cfg.Tty || !cfg.Stdin {
		t.Errorf("process config = %+v", cfg)
	}
	if !cfg.AutoRemove || !cfg.ReadOnly || cfg.Restart != "on-failure:2" {
		t.Errorf("lifecycle config = %+v", cfg)
	}
	if cfg.Hostname != shortID(resp.ID) {
		t.Errorf("hostname = %q, want the short id %q", cfg.Hostname, shortID(resp.ID))
	}
	if cfg.Resources.MemoryBytes != 4096 || cfg.Resources.CPUs != "0.5" || cfg.Resources.PidsLimit != 100 {
		t.Errorf("resources = %+v", cfg.Resources)
	}
	if !slices.Contains(cfg.Security.Capabilities, "CAP_NET_ADMIN") || slices.Contains(cfg.Security.Capabilities, "CAP_CHOWN") {
		t.Errorf("capabilities = %v", cfg.Security.Capabilities)
	}
	if cfg.Security.Seccomp != "default" || !cfg.Security.NoNewPrivs {
		t.Errorf("security = %+v", cfg.Security)
	}
	var bindMount, tmpfsMount *runtime.Mount
	for i := range cfg.Mounts {
		switch {
		case cfg.Mounts[i].Destination == "/data":
			bindMount = &cfg.Mounts[i]
		case cfg.Mounts[i].Destination == "/tmp":
			tmpfsMount = &cfg.Mounts[i]
		}
	}
	if bindMount == nil || bindMount.Type != "bind" || bindMount.Source != src || !bindMount.ReadOnly {
		t.Errorf("bind mount = %+v", cfg.Mounts)
	}
	if tmpfsMount == nil || tmpfsMount.Type != "tmpfs" {
		t.Errorf("tmpfs mount missing: %+v", cfg.Mounts)
	}
	if len(cfg.Network.Ports) != 1 || cfg.Network.Ports[0].HostPort != 8080 || cfg.Network.Ports[0].ContainerPort != 80 || cfg.Network.Ports[0].Protocol != "tcp" {
		t.Errorf("ports = %+v", cfg.Network.Ports)
	}
	if cfg.Network.Mode != "none" || len(cfg.Network.DNS) != 0 {
		t.Errorf("network = %+v", cfg.Network)
	}
	if got := b.labelsFor(resp.ID); got["team"] != "core" {
		t.Errorf("labels = %v", got)
	}
}

func TestCreateContainerNamedVolume(t *testing.T) {
	b := newTestBackend(t)
	seedImage(t, b)
	resp, err := b.CreateContainer(context.Background(), api.CreateRequest{
		Image:      "alpine:3.20",
		Cmd:        []string{"echo"},
		HostConfig: api.HostConfig{Binds: []string{"webdata:/site:ro"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := runtime.LoadConfig(filepath.Join(b.Store.Dir(resp.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mounts) != 1 || cfg.Mounts[0].Type != "bind" || !cfg.Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v", cfg.Mounts)
	}
	want := filepath.Join(b.Store.Root, "volumes", "webdata", "_data")
	if cfg.Mounts[0].Source != want {
		t.Errorf("volume source = %q, want %q", cfg.Mounts[0].Source, want)
	}
	if cfg.Mounts[0].Destination != "/site" {
		t.Errorf("destination = %q", cfg.Mounts[0].Destination)
	}
}

func TestCreateContainerHostnameAndErrors(t *testing.T) {
	b := newTestBackend(t)
	seedImage(t, b)
	// An explicit hostname is kept.
	resp, err := b.CreateContainer(context.Background(), api.CreateRequest{
		Image:    "alpine:3.20",
		Cmd:      []string{"echo"},
		Hostname: "custom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreateContainer(context.Background(), api.CreateRequest{
		Image:      "alpine:3.20",
		Cmd:        []string{"echo"},
		HostConfig: api.HostConfig{RestartPolicy: api.RestartPolicy{Name: "banana"}},
	}); err == nil {
		t.Error("unknown restart policy accepted")
	}
	if _, err := b.CreateContainer(context.Background(), api.CreateRequest{
		Image:      "alpine:3.20",
		Cmd:        []string{"echo"},
		HostConfig: api.HostConfig{NetworkMode: "container:abc"},
	}); err == nil {
		t.Error("unsupported network mode accepted")
	}
	cfg, err := runtime.LoadConfig(filepath.Join(b.Store.Dir(resp.ID), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "custom" {
		t.Errorf("hostname = %q, want custom", cfg.Hostname)
	}
}

func TestImagesListAndInspect(t *testing.T) {
	b := newTestBackend(t)
	seedImage(t, b)
	images, err := b.ListImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("images = %+v", images)
	}
	img := images[0]
	if !strings.HasPrefix(img.ID, "sha256:") || img.RepoTags[0] != "alpine:3.20" || img.Size != 1024 || img.VirtualSize != 1024 || img.Created == 0 {
		t.Errorf("image summary = %+v", img)
	}
	raw, err := b.InspectImage(context.Background(), "alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["ref"] != "docker.io/library/alpine:3.20" {
		t.Errorf("inspect image = %v", rec["ref"])
	}
	if _, err := b.InspectImage(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("missing image error = %v", err)
	}
}

func TestContainerLogsTailAndAll(t *testing.T) {
	b := newTestBackend(t)
	store, err := container.OpenAt(b.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &runtime.Config{Image: "alpine", Args: []string{"echo"}, Rootfs: runtime.Rootfs{Lower: []string{"/none"}, Overlay: true}}
	c, err := container.Create(store, cfg, "logtest")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(store.Dir(c.ID), "log")
	if err := os.WriteFile(log, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := b.ContainerLogs(ctx, c.ID, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != "three\nfour\n" {
		t.Errorf("tail 2 = %q, %v", data, err)
	}
	r, err = b.ContainerLogs(ctx, c.ID, false, -1)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(r)
	r.Close()
	if err != nil || string(data) != "one\ntwo\nthree\nfour\n" {
		t.Errorf("tail -1 = %q, %v", data, err)
	}
}

func TestContainerLogsFollowEndsAfterExit(t *testing.T) {
	b := newTestBackend(t)
	cfg := &runtime.Config{Image: "alpine", Args: []string{"echo"}, Rootfs: runtime.Rootfs{Lower: []string{"/none"}, Overlay: true}}
	c, err := container.Create(b.Store, cfg, "followtest")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Store.Dir(c.ID), "log"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.State.Status = container.StatusExited
	if err := c.SaveState(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := b.ContainerLogs(ctx, c.ID, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if string(data) != "hello\nworld\n" {
		t.Errorf("followed log = %q", data)
	}
}

func TestWaitContainerCancellation(t *testing.T) {
	b := newTestBackend(t)
	cfg := &runtime.Config{Image: "alpine", Args: []string{"sleep"}, Rootfs: runtime.Rootfs{Lower: []string{"/none"}, Overlay: true}}
	c, err := container.Create(b.Store, cfg, "waitme")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.WaitContainer(ctx, c.ID); err == nil {
		t.Error("WaitContainer should return on a cancelled context")
	}
}

func TestStopAndKillNotRunning(t *testing.T) {
	b := newTestBackend(t)
	cfg := &runtime.Config{Image: "alpine", Args: []string{"echo"}, Rootfs: runtime.Rootfs{Lower: []string{"/none"}, Overlay: true}}
	c, err := container.Create(b.Store, cfg, "quiet")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.StopContainer(context.Background(), c.ID, 0); err != nil {
		t.Errorf("stop on a created container = %v, want nil", err)
	}
	if err := b.KillContainer(context.Background(), c.ID, "SIGKILL"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("kill on a created container = %v, want a not-running error", err)
	}
	if err := b.KillContainer(context.Background(), c.ID, ""); err == nil {
		t.Error("kill with no signal should still fail on a stopped container")
	}
}

func TestPingAndVersion(t *testing.T) {
	b := newTestBackend(t)
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("ping = %v", err)
	}
	v, err := b.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "0.1.0" || v.APIVersion != "1.41" || v.GoVersion == "" {
		t.Errorf("version = %+v", v)
	}
	if v.Os != "linux" || v.Arch != goruntime.GOARCH || v.KernelVersion == "" {
		t.Errorf("version info = %+v", v)
	}
}
