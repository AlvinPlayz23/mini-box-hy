//go:build linux

// Package daemon adapts the minibox store and lifecycle onto the
// Docker-compatible API in internal/api, so `minibox serve` can expose a
// socket that Docker-style clients understand.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/api"
	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/security"
	"github.com/AlvinPlayz23/minibox/internal/volume"
	"golang.org/x/sys/unix"
)

// Backend implements api.Backend over a minibox state root.
type Backend struct {
	Store    *container.Store
	Images   *image.Store
	Progress func(string)
	// Network is the hook used when starting containers; when nil, containers
	// start with --network none.
	Network container.NetworkHook

	// labels are kept in memory keyed by container id: the runtime config has
	// nowhere to persist them, so they are lost on daemon restart and invisible
	// to other processes.
	mu      sync.Mutex
	labels  map[string]map[string]string
	running map[string]*container.Started
	starts  map[string]*sync.Mutex
}

// New returns a Backend for a state root, opening the stores.
func New(root string) (*Backend, error) {
	store, err := container.OpenAt(root)
	if err != nil {
		return nil, err
	}
	return &Backend{
		Store:   store,
		Images:  image.OpenStore(store),
		labels:  map[string]map[string]string{},
		running: map[string]*container.Started{},
		starts:  map[string]*sync.Mutex{},
	}, nil
}

// progress reports a message when a callback is installed.
func (b *Backend) progress(msg string) {
	if b.Progress != nil {
		b.Progress(msg)
	}
}

// startLock returns the per-container start mutex.
func (b *Backend) startLock(id string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.starts[id]; ok {
		return m
	}
	m := &sync.Mutex{}
	b.starts[id] = m
	return m
}

// Ping implements api.Backend.
func (b *Backend) Ping(ctx context.Context) error { return nil }

// Version implements api.Backend.
func (b *Backend) Version(ctx context.Context) (api.VersionInfo, error) {
	info := api.VersionInfo{
		Version:    "0.1.0",
		APIVersion: "1.41",
		GoVersion:  goruntime.Version(),
		Os:         goruntime.GOOS,
		Arch:       goruntime.GOARCH,
	}
	var uname unix.Utsname
	if err := unix.Uname(&uname); err == nil {
		info.KernelVersion = string(uname.Release[:bytesUntilNul(uname.Release[:])])
	}
	return info, nil
}

// bytesUntilNul trims a fixed-size C string.
func bytesUntilNul(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}

// ListContainers implements api.Backend.
func (b *Backend) ListContainers(ctx context.Context, all bool) ([]api.ContainerSummary, error) {
	ids, err := b.Store.List()
	if err != nil {
		return nil, err
	}
	out := []api.ContainerSummary{}
	for _, id := range ids {
		c, err := container.Load(b.Store, id)
		if err != nil {
			continue
		}
		if !all && c.State.Status != container.StatusRunning {
			continue
		}
		out = append(out, b.summary(c))
	}
	return out, nil
}

// summary builds the Docker-shaped listing entry for a container.
func (b *Backend) summary(c *container.Container) api.ContainerSummary {
	st := c.State
	name := st.Name
	if name == "" {
		name = shortID(st.ID)
	}
	s := api.ContainerSummary{
		ID:      st.ID,
		Names:   []string{"/" + name},
		Image:   st.Image,
		Command: st.Command,
		Created: st.CreatedAt.Unix(),
		State:   string(st.Status),
		Status:  statusText(st),
		Ports:   []api.PortSummary{},
		Labels:  b.labelsFor(st.ID),
		Health:  st.Health,
	}
	if cfg, err := runtime.LoadConfig(c.ConfigPath()); err == nil {
		for _, p := range cfg.Network.Ports {
			s.Ports = append(s.Ports, api.PortSummary{
				IP:          p.HostIP,
				PrivatePort: p.ContainerPort,
				PublicPort:  p.HostPort,
				Type:        orDefault(p.Protocol, "tcp"),
			})
		}
	}
	return s
}

// statusText renders a Docker-like human status line.
func statusText(st *container.State) string {
	switch st.Status {
	case container.StatusRunning:
		since := time.Since(st.StartedAt)
		if since < 0 {
			since = 0
		}
		return fmt.Sprintf("Up %s", humanDuration(since))
	case container.StatusExited:
		if st.FinishedAt.IsZero() {
			return fmt.Sprintf("Exited (%d)", st.ExitCode)
		}
		return fmt.Sprintf("Exited (%d) %s ago", st.ExitCode, humanDuration(time.Since(st.FinishedAt)))
	default:
		return string(st.Status)
	}
}

// humanDuration renders a duration the way `docker ps` does.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// labelsFor returns the labels recorded for a container.
func (b *Backend) labelsFor(id string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if labels, ok := b.labels[id]; ok {
		return labels
	}
	return map[string]string{}
}

// formatTime renders a timestamp the way Docker does.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.Format(time.RFC3339Nano)
}

// shortID renders the abbreviated container id used in messages.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// hostDNS returns the resolvers to write into containers.
func hostDNS() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				out = append(out, fields[1])
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1", "8.8.8.8"}
}

// InspectContainer implements api.Backend.
func (b *Backend) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return nil, err
	}
	cfg, err := runtime.LoadConfig(c.ConfigPath())
	if err != nil {
		return nil, err
	}
	st := c.State
	name := st.Name
	if name == "" {
		name = shortID(st.ID)
	}
	type stateJSON struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		OOMKilled  bool   `json:"OOMKilled"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     string `json:"Health,omitempty"`
	}
	type configJSON struct {
		Image      string            `json:"Image"`
		Cmd        []string          `json:"Cmd"`
		Env        []string          `json:"Env"`
		WorkingDir string            `json:"WorkingDir"`
		User       string            `json:"User"`
		Hostname   string            `json:"Hostname"`
		Tty        bool              `json:"Tty"`
		Labels     map[string]string `json:"Labels"`
	}
	type mountJSON struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	}
	type inspectJSON struct {
		ID              string            `json:"Id"`
		Name            string            `json:"Name"`
		Created         string            `json:"Created"`
		Image           string            `json:"Image"`
		State           stateJSON         `json:"State"`
		Config          configJSON        `json:"Config"`
		Mounts          []mountJSON       `json:"Mounts"`
		RestartCount    int               `json:"RestartCount"`
		Health          string            `json:"Health,omitempty"`
		NetworkSettings map[string]string `json:"NetworkSettings"`
		HostConfig      map[string]any    `json:"HostConfig"`
	}
	out := inspectJSON{
		ID:      st.ID,
		Name:    "/" + name,
		Created: st.CreatedAt.Format(time.RFC3339Nano),
		Image:   st.Image,
		State: stateJSON{
			Status:     string(st.Status),
			Running:    st.Status == container.StatusRunning,
			OOMKilled:  st.OOMKilled,
			Pid:        st.PID,
			ExitCode:   st.ExitCode,
			Error:      st.Error,
			Health:     st.Health,
			StartedAt:  formatTime(st.StartedAt),
			FinishedAt: formatTime(st.FinishedAt),
		},
		Config: configJSON{
			Image:      cfg.Image,
			Cmd:        cfg.Args,
			Env:        cfg.Env,
			WorkingDir: cfg.Cwd,
			User:       cfg.User,
			Hostname:   cfg.Hostname,
			Tty:        cfg.Tty,
			Labels:     b.labelsFor(st.ID),
		},
		Mounts:       []mountJSON{},
		RestartCount: st.Restarts,
		Health:       st.Health,
		NetworkSettings: map[string]string{
			"IPAddress":   cfg.Network.ContainerIP,
			"Gateway":     cfg.Network.Gateway,
			"NetworkMode": cfg.Network.Mode,
		},
		HostConfig: map[string]any{
			"Memory":         cfg.Resources.MemoryBytes,
			"PidsLimit":      cfg.Resources.PidsLimit,
			"AutoRemove":     cfg.AutoRemove,
			"ReadonlyRootfs": cfg.ReadOnly,
			"NetworkMode":    cfg.Network.Mode,
			"CapAdd":         cfg.Security.CapAdd,
			"CapDrop":        cfg.Security.CapDrop,
		},
	}
	for _, m := range cfg.Mounts {
		out.Mounts = append(out.Mounts, mountJSON{
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        map[bool]string{true: "ro", false: "rw"}[m.ReadOnly],
			RW:          !m.ReadOnly,
		})
	}
	return json.Marshal(out)
}

// CreateContainer implements api.Backend.
func (b *Backend) CreateContainer(ctx context.Context, req api.CreateRequest) (api.CreateResponse, error) {
	if req.Image == "" {
		return api.CreateResponse{}, fmt.Errorf("Image is required")
	}
	ref, err := image.ParseReference(req.Image)
	if err != nil {
		return api.CreateResponse{}, err
	}
	rec, err := b.ensureImage(ctx, ref)
	if err != nil {
		return api.CreateResponse{}, err
	}
	args, err := mergeCommand(rec.Config.Config, req.Entrypoint, req.Cmd)
	if err != nil {
		return api.CreateResponse{}, err
	}
	cfg := &runtime.Config{
		Image:    rec.Ref,
		Args:     args,
		Env:      mergeEnv(rec.Config.Config.Env, req.Env),
		Cwd:      firstNonEmpty(req.WorkingDir, rec.Config.Config.WorkingDir, "/"),
		User:     firstNonEmpty(req.User, rec.Config.Config.User),
		Hostname: req.Hostname,
		Tty:      req.Tty,
		// OpenStdin is recorded, but attached stdin is not streamed by the
		// API yet.
		Stdin:      req.OpenStdin,
		Detach:     true,
		AutoRemove: req.HostConfig.AutoRemove,
		ReadOnly:   req.HostConfig.ReadonlyRootfs,
		Rootfs:     runtime.Rootfs{Lower: []string{b.Images.LayerDir(rec.ChainID)}, Overlay: true},
		Created:    time.Now().UTC(),
	}
	mode, err := networkMode(req.HostConfig.NetworkMode)
	if err != nil {
		return api.CreateResponse{}, err
	}
	cfg.Network.Mode = mode
	if mode != "none" {
		cfg.Network.DNS = hostDNS()
	}
	for _, bind := range req.HostConfig.Binds {
		m, err := b.parseBind(bind)
		if err != nil {
			return api.CreateResponse{}, err
		}
		cfg.Mounts = append(cfg.Mounts, m)
	}
	for dest, opts := range req.HostConfig.Tmpfs {
		cfg.Mounts = append(cfg.Mounts, runtime.Mount{
			Type:        "tmpfs",
			Destination: dest,
			Options:     strings.Split(opts, ","),
		})
	}
	if err := applyPortBindings(cfg, req.HostConfig.PortBindings); err != nil {
		return api.CreateResponse{}, err
	}
	cfg.Resources.MemoryBytes = req.HostConfig.Memory
	if req.HostConfig.NanoCPUs > 0 {
		cfg.Resources.CPUs = strconv.FormatFloat(float64(req.HostConfig.NanoCPUs)/1e9, 'f', -1, 64)
	}
	if req.HostConfig.PidsLimit != nil {
		cfg.Resources.PidsLimit = *req.HostConfig.PidsLimit
	}
	caps, err := security.ResolveCapabilities(req.HostConfig.CapAdd, req.HostConfig.CapDrop)
	if err != nil {
		return api.CreateResponse{}, err
	}
	cfg.Security = runtime.Security{
		Capabilities: security.CapabilityNames(caps),
		CapAdd:       req.HostConfig.CapAdd,
		CapDrop:      req.HostConfig.CapDrop,
		Seccomp:      "default",
		NoNewPrivs:   true,
		Rootless:     os.Geteuid() != 0,
	}
	policy, err := restartString(req.HostConfig.RestartPolicy)
	if err != nil {
		return api.CreateResponse{}, err
	}
	cfg.Restart = policy

	c, err := container.Create(b.Store, cfg, "")
	if err != nil {
		return api.CreateResponse{}, err
	}
	// The id only exists after Create, so the default hostname is applied and
	// persisted here.
	if req.Hostname == "" {
		cfg.Hostname = shortID(c.ID)
	}
	cfg.Network.Hostname = cfg.Hostname
	if req.Hostname == "" {
		if err := cfg.Save(c.ConfigPath()); err != nil {
			_ = container.Remove(b.Store, c, true)
			return api.CreateResponse{}, fmt.Errorf("save container config: %w", err)
		}
	}
	if len(req.Labels) > 0 {
		b.mu.Lock()
		b.labels[c.ID] = req.Labels
		b.mu.Unlock()
	}
	return api.CreateResponse{ID: c.ID}, nil
}

// restartString renders a Docker restart policy as the CLI's flag value.
func restartString(p api.RestartPolicy) (string, error) {
	switch p.Name {
	case "", "no":
		return "no", nil
	case "always", "unless-stopped":
		return p.Name, nil
	case "on-failure":
		if p.MaximumRetryCount > 0 {
			return fmt.Sprintf("on-failure:%d", p.MaximumRetryCount), nil
		}
		return "on-failure", nil
	default:
		return "", fmt.Errorf("unsupported restart policy %q: use no, on-failure, always or unless-stopped", p.Name)
	}
}

// networkMode maps Docker network modes onto minibox modes.
func networkMode(mode string) (string, error) {
	switch mode {
	case "", "default":
		if os.Geteuid() == 0 {
			return "bridge", nil
		}
		return "pasta", nil
	case "none", "host", "bridge", "pasta":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported network mode %q: use none, host, bridge or pasta", mode)
	}
}

// applyPortBindings converts Docker port bindings into runtime mappings.
func applyPortBindings(cfg *runtime.Config, bindings map[string][]api.PortBinding) error {
	for key, list := range bindings {
		portPart, proto, _ := strings.Cut(key, "/")
		containerPort, err := strconv.Atoi(portPart)
		if err != nil || containerPort <= 0 || containerPort > 65535 {
			return fmt.Errorf("invalid port binding key %q: expected CONTAINER_PORT[/proto]", key)
		}
		if proto == "" {
			proto = "tcp"
		}
		for _, binding := range list {
			hostPort := 0
			if binding.HostPort != "" {
				n, err := strconv.Atoi(binding.HostPort)
				if err != nil || n < 0 || n > 65535 {
					return fmt.Errorf("invalid host port %q for %s", binding.HostPort, key)
				}
				hostPort = n
			}
			cfg.Network.Ports = append(cfg.Network.Ports, runtime.PortMapping{
				HostIP:        binding.HostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      proto,
			})
		}
	}
	return nil
}

// parseBind converts a Docker bind string into a runtime mount, resolving
// named volumes like the CLI does.
func (b *Backend) parseBind(spec string) (runtime.Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return runtime.Mount{}, fmt.Errorf("invalid bind %q: expected SRC:DST[:ro]", spec)
	}
	source := parts[0]
	m := runtime.Mount{Type: "bind", Destination: parts[1]}
	if len(parts) == 3 {
		if parts[2] != "ro" {
			return runtime.Mount{}, fmt.Errorf("invalid bind option %q in %q: only :ro is supported", parts[2], spec)
		}
		m.ReadOnly = true
	}
	if volume.IsName(source) {
		store, err := volume.Open(filepath.Join(b.Store.Root, "volumes"))
		if err != nil {
			return runtime.Mount{}, err
		}
		v, err := store.Ensure(source)
		if err != nil {
			return runtime.Mount{}, err
		}
		m.Source = v.Mountpoint
		return m, nil
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return runtime.Mount{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return runtime.Mount{}, fmt.Errorf("bind source %s: %w", abs, err)
	}
	m.Source = abs
	return m, nil
}

// ensureImage resolves an image, pulling it when it is missing.
func (b *Backend) ensureImage(ctx context.Context, ref image.Reference) (*image.Record, error) {
	if rec, err := b.Images.ResolveRecord(ref.Name()); err == nil {
		return rec, nil
	}
	b.progress(fmt.Sprintf("pulling %s", ref.Familiar()))
	return image.Pull(ctx, b.Images, ref, image.PullOptions{Progress: b.progress})
}

// mergeCommand combines an image's entrypoint/cmd with a request's overrides,
// following Docker's rules.
func mergeCommand(img image.ImageConfig, entrypoint, cmd []string) ([]string, error) {
	var argv []string
	argv = append(argv, img.Entrypoint...)
	if len(entrypoint) > 0 {
		argv = entrypoint
	}
	if len(cmd) > 0 {
		argv = append(argv, cmd...)
	} else {
		argv = append(argv, img.Cmd...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command: the image defines no entrypoint or cmd and the request supplied neither")
	}
	return argv, nil
}

// mergeEnv merges image environment variables with request overrides.
func mergeEnv(imageEnv, overrides []string) []string {
	merged := make([]string, 0, len(imageEnv)+len(overrides)+1)
	index := map[string]int{}
	put := func(kv string) {
		key, _, _ := strings.Cut(kv, "=")
		if i, ok := index[key]; ok {
			merged[i] = kv
			return
		}
		index[key] = len(merged)
		merged = append(merged, kv)
	}
	for _, kv := range imageEnv {
		put(kv)
	}
	for _, kv := range overrides {
		put(kv)
	}
	if _, ok := index["PATH"]; !ok {
		put("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return merged
}

// StartContainer implements api.Backend.
func (b *Backend) StartContainer(ctx context.Context, id string) error {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return err
	}
	if c.State.Status == container.StatusRunning {
		return fmt.Errorf("container %s is already running", shortID(c.ID))
	}
	lock := b.startLock(c.ID)
	lock.Lock()
	defer lock.Unlock()

	logFile, err := os.OpenFile(c.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open container log: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		logFile.Close()
		return err
	}
	started, err := container.Start(ctx, b.Store, c.ID, container.StartOptions{
		Stdin:    devNull,
		Stdout:   logFile,
		Stderr:   logFile,
		Network:  b.Network,
		Progress: b.progress,
		Detach:   true,
		Rootless: os.Geteuid() != 0,
	})
	devNull.Close()
	if err != nil {
		logFile.Close()
		return err
	}
	b.mu.Lock()
	b.running[c.ID] = started
	b.mu.Unlock()
	go func() {
		defer logFile.Close()
		_, _ = started.Wait(context.Background())
		b.mu.Lock()
		delete(b.running, c.ID)
		b.mu.Unlock()
	}()
	return nil
}

// StopContainer implements api.Backend.
func (b *Backend) StopContainer(ctx context.Context, id string, timeoutSeconds int) error {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return err
	}
	if c.State.Status != container.StatusRunning {
		return nil
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 10
	}
	return container.Stop(b.Store, c, time.Duration(timeoutSeconds)*time.Second)
}

// KillContainer implements api.Backend.
func (b *Backend) KillContainer(ctx context.Context, id, signal string) error {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return err
	}
	if c.State.PID <= 0 {
		return fmt.Errorf("container %s is not running", shortID(c.ID))
	}
	sig, err := parseSignal(signal)
	if err != nil {
		return err
	}
	if err := unix.Kill(c.State.PID, sig); err != nil {
		return fmt.Errorf("signal container %s: %w", shortID(c.ID), err)
	}
	return nil
}

// parseSignal maps Docker signal names and numbers onto signals.
func parseSignal(name string) (syscall.Signal, error) {
	if name == "" {
		return syscall.SIGTERM, nil
	}
	if n, err := strconv.Atoi(name); err == nil {
		if n <= 0 || n > 64 {
			return 0, fmt.Errorf("invalid signal number %d", n)
		}
		return syscall.Signal(n), nil
	}
	upper := strings.TrimPrefix(strings.ToUpper(name), "SIG")
	for sig := syscall.Signal(1); sig <= 31; sig++ {
		if signalName(sig) == upper {
			return sig, nil
		}
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}

// signalName returns the short name of a signal, without the SIG prefix.
func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGQUIT:
		return "QUIT"
	case syscall.SIGKILL:
		return "KILL"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGUSR1:
		return "USR1"
	case syscall.SIGUSR2:
		return "USR2"
	case syscall.SIGSTOP:
		return "STOP"
	case syscall.SIGCONT:
		return "CONT"
	case syscall.SIGWINCH:
		return "WINCH"
	default:
		return fmt.Sprintf("%d", int(sig))
	}
}

// RemoveContainer implements api.Backend.
func (b *Backend) RemoveContainer(ctx context.Context, id string, force bool) error {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return err
	}
	if err := container.Remove(b.Store, c, force); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.labels, c.ID)
	delete(b.starts, c.ID)
	b.mu.Unlock()
	return nil
}

// WaitContainer implements api.Backend.
func (b *Backend) WaitContainer(ctx context.Context, id string) (int, error) {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return 0, err
	}
	for {
		if c.State.Status == container.StatusExited {
			return c.State.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		next, err := container.Load(b.Store, c.ID)
		if err != nil {
			return 0, err
		}
		c = next
	}
}

// ContainerLogs implements api.Backend.
func (b *Backend) ContainerLogs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	c, err := container.Load(b.Store, id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(c.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return nil, err
	}
	if !follow {
		return &tailReader{file: f, tail: tail}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		defer f.Close()
		if tail > 0 {
			skipLines(f, tail)
		}
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					return
				}
				continue
			}
			if err != nil && err != io.EOF {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			if live, lerr := container.Load(b.Store, c.ID); lerr == nil && live.State.Status != container.StatusRunning {
				if n, _ := f.Read(buf); n > 0 {
					_, _ = pw.Write(buf[:n])
				}
				return
			}
		}
	}()
	return pr, nil
}

// tailReader returns the last N lines of a file.
type tailReader struct {
	file *os.File
	tail int
	once bool
}

// Read implements io.Reader.
func (t *tailReader) Read(p []byte) (int, error) {
	if !t.once {
		t.once = true
		if t.tail > 0 {
			skipLines(t.file, t.tail)
		}
	}
	return t.file.Read(p)
}

// Close implements io.Closer.
func (t *tailReader) Close() error { return t.file.Close() }

// skipLines seeks the file so that at most n lines remain to be read.
func skipLines(f *os.File, n int) {
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	block := int64(8192)
	offset := size
	seen := 0
	buf := make([]byte, block)
	for offset > 0 && seen <= n {
		readSize := block
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize
		if _, err := f.ReadAt(buf[:readSize], offset); err != nil && err != io.EOF {
			return
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				seen++
				if seen > n {
					_, _ = f.Seek(offset+i+1, io.SeekStart)
					return
				}
			}
		}
	}
	_, _ = f.Seek(0, io.SeekStart)
}

// ListImages implements api.Backend.
func (b *Backend) ListImages(ctx context.Context) ([]api.ImageSummary, error) {
	records, err := b.Images.ListRecords()
	if err != nil {
		return nil, err
	}
	out := []api.ImageSummary{}
	for _, rec := range records {
		ref, err := image.ParseReference(rec.Ref)
		if err != nil {
			continue
		}
		id := rec.Digest
		if id == "" {
			id = rec.ChainID
		}
		if id != "" && !strings.HasPrefix(id, "sha256:") {
			id = "sha256:" + id
		}
		out = append(out, api.ImageSummary{
			ID:          id,
			RepoTags:    []string{ref.Familiar()},
			Created:     rec.Created.Unix(),
			Size:        rec.Size,
			VirtualSize: rec.Size,
		})
	}
	return out, nil
}

// InspectImage implements api.Backend.
func (b *Backend) InspectImage(ctx context.Context, name string) (json.RawMessage, error) {
	rec, err := b.Images.ResolveRecord(name)
	if err != nil {
		return nil, fmt.Errorf("no such image %q: %w", name, err)
	}
	return json.Marshal(rec)
}

// PullImage implements api.Backend.
func (b *Backend) PullImage(ctx context.Context, ref string, progress func(string)) error {
	parsed, err := image.ParseReference(ref)
	if err != nil {
		return err
	}
	_, err = image.Pull(ctx, b.Images, parsed, image.PullOptions{Progress: progress})
	return err
}
