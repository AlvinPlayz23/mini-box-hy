//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/security"
)

// runOptions collects the parsed `minibox run` flags.
type runOptions struct {
	detach     bool
	autoRm     bool
	name       string
	tty        bool
	stdin      bool
	env        stringList
	volumes    stringList
	workdir    string
	user       string
	publish    stringList
	network    string
	hostname   string
	memory     string
	memorySwap string
	cpus       string
	pidsLimit  string
	readOnly   bool
	initProc   bool
	capAdd     stringList
	capDrop    stringList
	seccomp    string
	platform   string
	dryRun     bool
	image      string
	command    []string
}

// parseRunFlags parses run flags without consuming IMAGE/CMD.
func parseRunFlags(args []string, name string) (*runOptions, error) {
	o := &runOptions{network: "", seccomp: "default"}
	fs := newFlagSet(name)
	fs.BoolVar(&o.detach, "d", false, "")
	fs.BoolVar(&o.detach, "detach", false, "")
	fs.BoolVar(&o.autoRm, "rm", false, "")
	fs.StringVar(&o.name, "name", "", "")
	fs.BoolVar(&o.tty, "t", false, "")
	fs.BoolVar(&o.stdin, "i", false, "")
	fs.Var(&o.env, "e", "")
	fs.Var(&o.env, "env", "")
	fs.Var(&o.volumes, "v", "")
	fs.Var(&o.volumes, "volume", "")
	fs.StringVar(&o.workdir, "w", "", "")
	fs.StringVar(&o.workdir, "workdir", "", "")
	fs.StringVar(&o.user, "u", "", "")
	fs.StringVar(&o.user, "user", "", "")
	fs.Var(&o.publish, "p", "")
	fs.Var(&o.publish, "publish", "")
	fs.StringVar(&o.network, "network", "", "")
	fs.StringVar(&o.hostname, "hostname", "", "")
	fs.StringVar(&o.memory, "memory", "", "")
	fs.StringVar(&o.memorySwap, "memory-swap", "", "")
	fs.StringVar(&o.cpus, "cpus", "", "")
	fs.StringVar(&o.pidsLimit, "pids-limit", "", "")
	fs.BoolVar(&o.readOnly, "read-only", false, "")
	fs.BoolVar(&o.initProc, "init", false, "")
	fs.Var(&o.capAdd, "cap-add", "")
	fs.Var(&o.capDrop, "cap-drop", "")
	fs.StringVar(&o.seccomp, "seccomp", "default", "")
	fs.StringVar(&o.platform, "platform", "", "")
	fs.BoolVar(&o.dryRun, "dry-run", false, "")

	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return nil, usageError("run needs an image: `minibox run alpine echo hi`")
	}
	o.image = rest[0]
	o.command = rest[1:]
	// A bare "-it" is the usual spelling; accept "-i -t" as well.
	for _, a := range args {
		if a == "-it" || a == "--it" {
			o.tty, o.stdin = true, true
		}
	}
	return o, nil
}

// runCommand implements `minibox run`.
func runCommand(env *Env, args []string) int {
	opts, err := parseRunFlags(args, "run")
	if err != nil {
		return reportError(env, err)
	}
	ctx := context.Background()
	cfg, err := buildConfig(ctx, env, opts)
	if err != nil {
		return reportError(env, err)
	}
	c, err := container.Create(env.Store, cfg, opts.name)
	if err != nil {
		return reportError(env, err)
	}
	if opts.dryRun {
		if err := printConfig(env, c); err != nil {
			return reportError(env, err)
		}
		_ = container.Remove(env.Store, c, true)
		return 0
	}
	if opts.detach {
		if err := startDetached(env, c.ID, cfg); err != nil {
			return reportError(env, err)
		}
		fmt.Fprintln(env.Stdout, c.ID)
		return 0
	}
	code, err := runForeground(ctx, env, c.ID, opts)
	if err != nil {
		return reportError(env, err)
	}
	return code
}

// runForeground starts a container and supervises it in this process.
func runForeground(ctx context.Context, env *Env, id string, opts *runOptions) (int, error) {
	startOpts := container.StartOptions{
		Stdin:    env.Stdin,
		Stdout:   env.StdoutFile,
		Stderr:   env.StderrFile,
		TTY:      opts.tty && isTerminal(env.Stdin),
		Network:  networkHook(),
		Progress: env.Progress,
		Detach:   false,
		Rootless: os.Geteuid() != 0,
	}
	var started *container.Started
	started, err := container.Start(ctx, env.Store, id, startOpts)
	if err != nil {
		cleanupFailedStart(env, id)
		return 1, err
	}
	if started.PTY != nil {
		term, terr := container.MakeRaw(env.Stdin)
		if terr == nil {
			defer term.Restore()
		}
		rows, cols := container.TerminalSize(env.Stdin)
		_ = started.PTY.SetWinsize(rows, cols)
		go container.CopyPTY(started.PTY.Master, env.Stdin, env.StdoutFile)
	}
	code, err := started.Wait(ctx)
	if err != nil {
		return code, err
	}
	return code, nil
}

// cleanupFailedStart removes a container that never started.
func cleanupFailedStart(env *Env, id string) {
	c, err := container.Load(env.Store, id)
	if err != nil {
		return
	}
	c.State.Status = container.StatusDead
	c.State.FinishedAt = time.Now().UTC()
	_ = c.SaveState()
	if s, err := container.Load(env.Store, id); err == nil {
		_ = container.Remove(env.Store, s, true)
	}
}

// buildConfig turns run flags plus an image record into a runtime config.
func buildConfig(ctx context.Context, env *Env, opts *runOptions) (*runtime.Config, error) {
	// Validate flags that do not need the image before any network work.
	netCfg, err := networkConfig(opts)
	if err != nil {
		return nil, err
	}
	ref, err := image.ParseReference(opts.image)
	if err != nil {
		return nil, err
	}
	rec, err := ensureImage(ctx, env, ref, opts.platform)
	if err != nil {
		return nil, err
	}

	args, err := resolveCommand(rec.Config.Config, opts.command)
	if err != nil {
		return nil, err
	}
	cfg := &runtime.Config{
		Image:      rec.Ref,
		Args:       args,
		Env:        mergeEnv(rec.Config.Config.Env, opts.env),
		Cwd:        firstNonEmpty(opts.workdir, rec.Config.Config.WorkingDir, "/"),
		User:       firstNonEmpty(opts.user, rec.Config.Config.User),
		Tty:        opts.tty,
		Stdin:      opts.stdin,
		Detach:     opts.detach,
		AutoRemove: opts.autoRm,
		ReadOnly:   opts.readOnly,
		Init:       opts.initProc,
		Rootfs: runtime.Rootfs{
			Lower:   []string{env.Images.LayerDir(rec.ChainID)},
			Overlay: true,
		},
	}
	cfg.Hostname = opts.hostname
	if cfg.Hostname == "" {
		cfg.Hostname = "minibox"
	}
	cfg.Network.Hostname = cfg.Hostname

	if err := applyResources(cfg, opts); err != nil {
		return nil, err
	}
	if err := applySecurity(cfg, opts); err != nil {
		return nil, err
	}
	if err := applyMounts(cfg, opts); err != nil {
		return nil, err
	}
	cfg.Network.Mode = netCfg.Mode
	cfg.Network.Ports = netCfg.Ports
	cfg.Network.DNS = hostDNS()
	return cfg, nil
}

// ensureImage returns the stored image, pulling it when it is missing.
func ensureImage(ctx context.Context, env *Env, ref image.Reference, platform string) (*image.Record, error) {
	rec, err := env.Images.ResolveRecord(ref.Name())
	if err == nil {
		return rec, nil
	}
	opts := image.PullOptions{Progress: env.Progress}
	if platform != "" {
		p, perr := image.ParsePlatform(platform)
		if perr != nil {
			return nil, perr
		}
		opts.Platform = p
	}
	env.Progress(fmt.Sprintf("image %s not found locally, pulling", ref.Familiar()))
	return image.Pull(ctx, env.Images, ref, opts)
}

// resolveCommand merges the image entrypoint/cmd with the command line.
func resolveCommand(img image.ImageConfig, userArgs []string) ([]string, error) {
	var argv []string
	argv = append(argv, img.Entrypoint...)
	if len(userArgs) > 0 {
		argv = append(argv, userArgs...)
	} else {
		argv = append(argv, img.Cmd...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command given and the image defines no entrypoint or cmd: " +
			"pass a command, for example `minibox run alpine echo hi`")
	}
	return argv, nil
}

// mergeEnv combines image environment variables with -e overrides.
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
		if !strings.ContainsRune(kv, '=') {
			if v, ok := os.LookupEnv(kv); ok {
				kv = kv + "=" + v
			}
		}
		put(kv)
	}
	if _, ok := index["PATH"]; !ok {
		put("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return merged
}

// applyResources translates resource flags into the config.
func applyResources(cfg *runtime.Config, opts *runOptions) error {
	if opts.memory != "" {
		n, err := cgroup.ParseBytes(opts.memory)
		if err != nil {
			return err
		}
		cfg.Resources.MemoryBytes = n
	}
	if opts.memorySwap != "" {
		if strings.EqualFold(opts.memorySwap, "-1") {
			cfg.Resources.MemorySwapBytes = -1
		} else {
			n, err := cgroup.ParseBytes(opts.memorySwap)
			if err != nil {
				return err
			}
			cfg.Resources.MemorySwapBytes = n
		}
	}
	if opts.cpus != "" {
		if _, err := cgroup.ParseCPUs(opts.cpus); err != nil {
			return err
		}
		cfg.Resources.CPUs = opts.cpus
	}
	if opts.pidsLimit != "" {
		n, err := cgroup.ParsePids(opts.pidsLimit)
		if err != nil {
			return err
		}
		cfg.Resources.PidsLimit = n
	}
	return nil
}

// applySecurity translates capability and seccomp flags into the config.
func applySecurity(cfg *runtime.Config, opts *runOptions) error {
	caps, err := security.ResolveCapabilities(opts.capAdd, opts.capDrop)
	if err != nil {
		return err
	}
	cfg.Security = runtime.Security{
		Capabilities: security.CapabilityNames(caps),
		CapAdd:       opts.capAdd,
		CapDrop:      opts.capDrop,
		Seccomp:      opts.seccomp,
		NoNewPrivs:   true,
		Rootless:     os.Geteuid() != 0,
	}
	switch opts.seccomp {
	case "default", "unconfined":
	default:
		return usageError("invalid --seccomp %q: use default or unconfined", opts.seccomp)
	}
	return nil
}

// applyMounts translates -v flags into mounts.
func applyMounts(cfg *runtime.Config, opts *runOptions) error {
	for _, spec := range opts.volumes {
		m, err := parseVolume(spec)
		if err != nil {
			return err
		}
		cfg.Mounts = append(cfg.Mounts, m)
	}
	return nil
}

// parseVolume parses SRC:DST[:ro] and tmpfs:DST[:opts].
func parseVolume(spec string) (runtime.Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return runtime.Mount{}, usageError("invalid volume %q: expected SRC:DST[:ro]", spec)
	}
	if parts[0] == "tmpfs" {
		m := runtime.Mount{Type: "tmpfs", Destination: parts[1]}
		if len(parts) > 2 && parts[2] != "" && parts[2] != "ro" {
			m.Options = strings.Split(parts[2], ",")
		}
		return m, nil
	}
	src, err := filepath.Abs(parts[0])
	if err != nil {
		return runtime.Mount{}, usageError("invalid volume source %q: %v", parts[0], err)
	}
	if _, err := os.Stat(src); err != nil {
		return runtime.Mount{}, fmt.Errorf("volume source %s: %w", src, err)
	}
	m := runtime.Mount{Type: "bind", Source: src, Destination: parts[1]}
	if len(parts) > 2 {
		if parts[2] != "ro" {
			return runtime.Mount{}, usageError("invalid volume option %q: only :ro is supported", parts[2])
		}
		m.ReadOnly = true
	}
	return m, nil
}

// isTerminal reports whether a file is a terminal.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// printConfig writes the resolved container config as JSON. --dry-run uses it,
// which makes the whole resolution pipeline testable without namespaces.
func printConfig(env *Env, c *container.Container) error {
	cfg, err := runtime.LoadConfig(c.ConfigPath())
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, string(data))
	return nil
}

// runRawCommand implements the M1 development command: run a command inside a
// freshly prepared root filesystem with namespaces, without an image store.
func runRawCommand(env *Env, args []string) int {
	fs := newFlagSet("run-raw")
	hostname := fs.String("hostname", "minibox", "container hostname")
	network := fs.String("network", "none", "network mode")
	rootfs := fs.String("rootfs", "", "root filesystem directory")
	if err := parseFlags(fs, args); err != nil {
		return reportError(env, err)
	}
	rest := fs.Args()
	if *rootfs == "" {
		if len(rest) < 2 {
			return reportError(env, usageError("usage: minibox run-raw ROOTFS CMD..."))
		}
		*rootfs = rest[0]
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return reportError(env, usageError("usage: minibox run-raw ROOTFS CMD..."))
	}
	abs, err := filepath.Abs(*rootfs)
	if err != nil {
		return reportError(env, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return reportError(env, fmt.Errorf("rootfs %s: %w", abs, err))
	}
	caps, err := security.ResolveCapabilities(nil, nil)
	if err != nil {
		return reportError(env, err)
	}
	cfg := &runtime.Config{
		Image:    "run-raw:" + abs,
		Args:     rest,
		Env:      []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Cwd:      "/",
		Hostname: *hostname,
		Rootfs:   runtime.Rootfs{Merged: abs, Lower: []string{abs}, Overlay: false},
		Network:  runtime.Network{Mode: *network, Hostname: *hostname},
		Security: runtime.Security{
			Capabilities: security.CapabilityNames(caps),
			Seccomp:      "default",
			NoNewPrivs:   true,
			Rootless:     os.Geteuid() != 0,
		},
	}
	c, err := container.Create(env.Store, cfg, "")
	if err != nil {
		return reportError(env, err)
	}
	opts := &runOptions{network: *network}
	code, err := runForeground(context.Background(), env, c.ID, opts)
	if err != nil {
		return reportError(env, err)
	}
	return code
}
