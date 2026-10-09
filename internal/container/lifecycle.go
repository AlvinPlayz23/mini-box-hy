//go:build linux

package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/storage"
	"golang.org/x/sys/unix"
)

// NetworkRequest describes the networking work for one container.
type NetworkRequest struct {
	ContainerID string
	PID         int
	Config      runtime.Network
	StateDir    string
}

// NetworkHook is implemented by the network package; it lets the container
// package stay independent of it.
type NetworkHook interface {
	// Setup configures networking and returns the updated network config plus
	// a cleanup function.
	Setup(req NetworkRequest) (runtime.Network, func() error, error)
	// Teardown removes networking for a container, best effort.
	Teardown(cfg runtime.Network, stateDir string) error
}

// StartOptions controls how a container is started.
type StartOptions struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	// TTY requests a pseudo-terminal for the container process.
	TTY bool
	// Network is optional; without it the container gets no networking.
	Network NetworkHook
	// Progress receives human-readable setup messages.
	Progress func(string)
	// NoCgroup skips cgroup setup entirely.
	NoCgroup bool
	// Detach runs the container in its own session.
	Detach bool
	// Rootless enables user-namespace handling.
	Rootless bool
}

// Started is a running container.
type Started struct {
	ID     string
	Config *runtime.Config
	Init   *runtime.InitProcess
	Cgroup *cgroup.Cgroup
	Rootfs storage.RootfsPaths
	PTY    *PTY
	// stopHealth stops the health probe loop, if one is running.
	stopHealth func()
	cleanup    []func() error
	network    runtime.Network
	hadNet     bool
	progress   func(string)
}

// Create allocates a container directory, writes config.json and state.json and
// returns the new container.
func Create(store *Store, cfg *runtime.Config, name string) (*Container, error) {
	lock, err := store.LockStore()
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	if name != "" {
		if existing, err := store.findByName(name); err == nil && existing != "" {
			return nil, fmt.Errorf("a container named %q already exists (%s): choose another --name or remove it first", name, existing[:12])
		}
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	dir := store.Dir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create container directory: %w", err)
	}
	cfg.ID = id
	if cfg.Network.ContainerID == "" {
		cfg.Network.ContainerID = id
	}
	if cfg.Created.IsZero() {
		cfg.Created = time.Now().UTC()
	}
	c := &Container{ID: id, Dir: dir}
	if err := cfg.Save(c.ConfigPath()); err != nil {
		return nil, err
	}
	st := &State{
		ID:        id,
		Name:      name,
		Image:     cfg.Image,
		Command:   commandString(cfg.Args),
		Status:    StatusCreated,
		CreatedAt: cfg.Created,
	}
	c.State = st
	if err := c.SaveState(); err != nil {
		return nil, err
	}
	return c, nil
}

// Load returns a container by id prefix or name.
func Load(store *Store, idOrName string) (*Container, error) {
	id, err := store.Resolve(idOrName)
	if err != nil {
		return nil, err
	}
	dir := store.Dir(id)
	c := &Container{ID: id, Dir: dir}
	st, err := LoadState(c.StatePath())
	if err != nil {
		return nil, err
	}
	st.Refresh()
	c.State = st
	return c, nil
}

// Start prepares the root filesystem, cgroup and namespaces for an existing
// container and runs it up to the point where the user command is exec'd.
func Start(ctx context.Context, store *Store, id string, opts StartOptions) (*Started, error) {
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	c, err := Load(store, id)
	if err != nil {
		return nil, err
	}
	if c.State.Status == StatusRunning {
		return nil, fmt.Errorf("container %s is already running", shortID(id))
	}
	cfg, err := runtime.LoadConfig(c.ConfigPath())
	if err != nil {
		return nil, err
	}

	lock, err := store.LockContainer(id)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	s := &Started{ID: id, Config: cfg, progress: progress}

	// Root filesystem: overlay mount over the image's layer chain, or the
	// already-prepared directory used by `run-raw`.
	var rootfs storage.RootfsPaths
	if !cfg.Rootfs.Overlay && len(cfg.Rootfs.Lower) == 0 && cfg.Rootfs.Merged != "" {
		rootfs = storage.RootfsPaths{Merged: cfg.Rootfs.Merged}
	} else {
		rootfs, err = storage.PrepareRootfs(c.Dir, cfg.Rootfs.Lower, storage.RootfsOptions{
			Overlay:  cfg.Rootfs.Overlay,
			Rootless: opts.Rootless,
		})
	}
	if err != nil {
		s.cleanupAll()
		return nil, err
	}
	s.Rootfs = rootfs
	cfg.Rootfs.Merged = rootfs.Merged
	cfg.Rootfs.Upper = rootfs.Upper
	cfg.Rootfs.Work = rootfs.Work
	cfg.Network.MergedRootfs = rootfs.Merged
	if err := cfg.Save(c.ConfigPath()); err != nil {
		s.cleanupAll()
		return nil, err
	}

	// Cgroup with resource limits.
	limits := cgroup.Limits{
		MemoryBytes:     cfg.Resources.MemoryBytes,
		MemorySwapBytes: cfg.Resources.MemorySwapBytes,
		PidsMax:         cfg.Resources.PidsLimit,
	}
	if cfg.Resources.CPUs != "" {
		quota, err := cgroup.ParseCPUs(cfg.Resources.CPUs)
		if err != nil {
			s.cleanupAll()
			return nil, err
		}
		limits.CPUQuotaMicros = quota
	}
	cgroupFD := -1
	var cgroupProcs string
	if !opts.NoCgroup {
		mgr, err := cgroup.Detect()
		switch {
		case err == nil:
			cg, err := mgr.Create(id)
			if err != nil {
				s.cleanupAll()
				return nil, err
			}
			s.Cgroup = cg
			s.cleanup = append(s.cleanup, func() error { return cg.RemoveIfEmpty() })
			if fd, err := cg.FD(); err == nil {
				cgroupFD = fd
				defer unix.Close(fd)
			}
			cgroupProcs = filepath.Join(cg.Path, "cgroup.procs")
		case limitsRequested(limits):
			s.cleanupAll()
			return nil, fmt.Errorf("%w (needed for --memory/--cpus/--pids-limit)", cgroup.ErrNoV2)
		default:
			progress("warning: cgroup v2 is unavailable, resource limits and accounting are disabled")
		}
	}

	// Terminal handling.
	stdin, stdout, stderr := opts.Stdin, opts.Stdout, opts.Stderr
	if opts.TTY {
		pty, err := OpenPTY()
		if err != nil {
			s.cleanupAll()
			return nil, err
		}
		s.PTY = pty
		s.cleanup = append(s.cleanup, func() error { return pty.Close() })
		stdin, stdout, stderr = pty.Slave, pty.Slave, pty.Slave
	}

	startOpts := runtime.StartOptions{
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		CgroupFD:    cgroupFD,
		CgroupProcs: cgroupProcs,
		HostNetwork: cfg.Network.Mode == "host",
		Detach:      opts.Detach,
		Rootless:    opts.Rootless,
	}
	if opts.Rootless {
		if uids, gids, ok := runtime.SubIDAvailable(); ok {
			startOpts.MultiIDMappings = true
			startOpts.SubUIDs = uids
			startOpts.SubGIDs = gids
		} else {
			startOpts.SingleIDMapping = true
		}
	}

	initProc, err := runtime.StartInit(c.ConfigPath(), startOpts)
	if err != nil {
		s.cleanupAll()
		return nil, err
	}
	s.Init = initProc
	s.cleanup = append(s.cleanup, func() error { initProc.Kill(); return nil })

	if err := initProc.WaitReady(30 * time.Second); err != nil {
		s.cleanupAll()
		return nil, err
	}
	progress(fmt.Sprintf("container %s started (pid %d)", shortID(id), initProc.PID()))

	// Fallback placement plus limits, both before the container is released.
	if s.Cgroup != nil {
		if err := s.Cgroup.AddPid(initProc.PID()); err != nil && cgroupProcs == "" {
			s.cleanupAll()
			return nil, err
		}
		if err := s.Cgroup.SetLimits(limits); err != nil {
			_ = initProc.Fail(err.Error())
			s.cleanupAll()
			return nil, err
		}
	}

	// Networking, before the user command runs.
	if opts.Network != nil {
		net, cleanup, err := opts.Network.Setup(NetworkRequest{
			ContainerID: id,
			PID:         initProc.PID(),
			Config:      cfg.Network,
			StateDir:    c.Dir,
		})
		if err != nil {
			_ = initProc.Fail(err.Error())
			s.cleanupAll()
			return nil, err
		}
		s.network = net
		s.hadNet = true
		cfg.Network = net
		if err := cfg.Save(c.ConfigPath()); err != nil {
			s.cleanupAll()
			return nil, err
		}
		if cleanup != nil {
			s.cleanup = append(s.cleanup, cleanup)
		}
	}

	if err := initProc.Go(); err != nil {
		s.cleanupAll()
		return nil, err
	}
	c.State.Status = StatusRunning
	c.State.PID = initProc.PID()
	c.State.StartedAt = time.Now().UTC()
	if cfg.HealthCheck != nil {
		s.stopHealth = startHealthMonitor(store, id, cfg)
	}
	if err := c.SaveState(); err != nil {
		s.cleanupAll()
		return nil, err
	}
	return s, nil
}

// Wait blocks until the container exits, then tears everything down and
// records the exit status.
func (s *Started) Wait(ctx context.Context) (int, error) {
	if s.stopHealth != nil {
		defer s.stopHealth()
	}
	code, err := s.Init.Wait()
	oom := false
	if s.Cgroup != nil {
		if events, eerr := s.Cgroup.Events(); eerr == nil {
			oom = events.OOMKill > 0
		}
	}
	if werr := s.teardown(); werr != nil && err == nil {
		err = werr
	}
	if store := s.storeFromConfig(); store != nil {
		if c, lerr := Load(store, s.ID); lerr == nil {
			c.State.Status = StatusExited
			c.State.ExitCode = code
			c.State.OOMKilled = oom
			c.State.PID = 0
			c.State.FinishedAt = time.Now().UTC()
			_ = c.SaveState()
			if s.Config.AutoRemove {
				_ = teardownContainer(store, c)
				_ = store.Remove(s.ID)
			}
		}
	}
	return code, err
}

// storeFromConfig recovers the store root recorded in the container config so
// that Wait can update state without holding a store reference.
func (s *Started) storeFromConfig() *Store {
	if s.Config == nil {
		return nil
	}
	root := os.Getenv(RootEnv)
	if root == "" {
		root = DefaultRoot()
	}
	st, err := OpenAt(root)
	if err != nil {
		return nil
	}
	return st
}

// teardown releases networking, cgroup and rootfs resources.
func (s *Started) teardown() error {
	var firstErr error
	if s.hadNet && s.network.Mode != "" {
		stateDir := ""
		if st := s.storeFromConfig(); st != nil {
			stateDir = st.Dir(s.ID)
		}
		if err := teardownNetwork(s.network, stateDir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := s.cleanupAll(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// cleanupAll runs the registered cleanup functions in reverse order.
func (s *Started) cleanupAll() error {
	var firstErr error
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		if err := s.cleanup[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.cleanup = nil
	return firstErr
}

// teardownNetwork is set by the cli layer through SetNetworkTeardown so that
// the container package does not import the network package.
var networkTeardown func(cfg runtime.Network, stateDir string) error

// SetNetworkTeardown installs the network teardown implementation.
func SetNetworkTeardown(fn func(cfg runtime.Network, stateDir string) error) {
	networkTeardown = fn
}

func teardownNetwork(cfg runtime.Network, stateDir string) error {
	if networkTeardown == nil {
		return nil
	}
	return networkTeardown(cfg, stateDir)
}

// teardownContainer removes container leftovers: mounts, cgroup, network.
func teardownContainer(store *Store, c *Container) error {
	var firstErr error
	cfg, err := runtime.LoadConfig(c.ConfigPath())
	if err == nil {
		rootfs := storage.RootfsPaths{
			Merged:  cfg.Rootfs.Merged,
			Upper:   cfg.Rootfs.Upper,
			Work:    cfg.Rootfs.Work,
			Overlay: cfg.Rootfs.Overlay,
		}
		if err := storage.TeardownRootfs(rootfs); err != nil {
			firstErr = err
		}
		if cfg.Network.Mode != "" {
			_ = teardownNetwork(cfg.Network, c.Dir)
		}
	}
	if mgr, err := cgroup.Detect(); err == nil {
		if cg, err := mgr.Open(c.ID); err == nil {
			_ = cg.Kill()
			_ = cg.Remove()
		}
	}
	return firstErr
}

// Stop terminates a running container: SIGTERM, then SIGKILL after the grace
// period.
func Stop(store *Store, c *Container, timeout time.Duration) error {
	if c.State.Status != StatusRunning || c.State.PID <= 0 {
		return nil
	}
	pid := c.State.PID
	if err := unix.Kill(pid, unix.SIGTERM); err != nil && err != unix.ESRCH {
		return fmt.Errorf("send SIGTERM to %s: %w", shortID(c.ID), err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		return fmt.Errorf("send SIGKILL to %s: %w", shortID(c.ID), err)
	}
	for i := 0; i < 100; i++ {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("container %s did not stop", shortID(c.ID))
}

// Remove deletes a container and all of its resources.
func Remove(store *Store, c *Container, force bool) error {
	if c.State.Status == StatusRunning {
		if !force {
			return fmt.Errorf("container %s is running: stop it first or use -f to force removal", shortID(c.ID))
		}
		if err := Stop(store, c, 10*time.Second); err != nil {
			return err
		}
	}
	if err := teardownContainer(store, c); err != nil {
		return err
	}
	return store.Remove(c.ID)
}

// limitsRequested reports whether any resource limit was asked for.
func limitsRequested(l cgroup.Limits) bool {
	return l.MemoryBytes > 0 || l.MemorySwapBytes > 0 || l.CPUQuotaMicros > 0 || l.PidsMax > 0
}

// shortID renders the abbreviated container id used in messages.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// commandString renders the container command for display.
func commandString(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// WriteStateFile is a helper for the shim and for tests.
func WriteStateFile(path string, st *State) error { return st.Save(path) }

// ParsePID reads a pid from a file (used by the shim handshake).
func ParsePID(data string) (int, error) {
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid pid %q", string(data))
	}
	return pid, nil
}
