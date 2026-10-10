//go:build linux

package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/AlvinPlayz23/minibox/internal/security"
	"golang.org/x/sys/unix"
)

// LoadConfig reads a container config from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read container config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse container config %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes a container config atomically.
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write container config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write container config: %w", err)
	}
	return nil
}

// Init is the entry point of the container init process (the hidden `minibox
// init` subcommand). It runs inside all container namespaces, prepares the
// root filesystem and security context, synchronises with the supervisor and
// finally execs the user command. It only returns on error.
func Init() error {
	syncFD, err := strconv.Atoi(os.Getenv(EnvSyncFD))
	if err != nil {
		return fmt.Errorf("%s is not set: `minibox init` is an internal command", EnvSyncFD)
	}
	sync := os.NewFile(uintptr(syncFD), "minibox-sync")
	report := func(err error) error {
		if sync != nil {
			fmt.Fprintf(sync, "%s %s\n", errToken, strings.ReplaceAll(err.Error(), "\n", " "))
		}
		return err
	}
	// Anything that kills this process before it can speak used to reach the
	// supervisor as a bare "container init exited during setup: EOF". A panic
	// now reports itself first, so the failure is legible.
	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 8<<10)
			stack = stack[:runtime.Stack(stack, false)]
			trace := strings.ReplaceAll(string(stack), "\n", " | ")
			if len(trace) > 600 {
				trace = trace[:600] + " ..."
			}
			_ = report(fmt.Errorf("panic during container setup: %v (stack: %s)", r, trace))
		}
	}()

	configPath := os.Getenv(EnvConfig)
	if configPath == "" {
		return report(fmt.Errorf("%s is not set: `minibox init` is an internal command", EnvConfig))
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return report(err)
	}

	// Fallback cgroup placement for kernels without CLONE_INTO_CGROUP.
	if procs := os.Getenv(EnvCgroupProcs); procs != "" {
		if err := os.WriteFile(procs, []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
			return report(fmt.Errorf("join cgroup %s: %w", procs, err))
		}
	}

	if err := setupRootfs(cfg); err != nil {
		return report(err)
	}

	if cfg.Hostname != "" {
		if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
			return report(fmt.Errorf("set hostname to %q: %w", cfg.Hostname, err))
		}
	}
	if err := bringUpLoopback(); err != nil && os.Getenv("MINIBOX_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "minibox: could not enable loopback: %v\n", err)
	}

	// From here on, per-thread state (uid, capabilities, seccomp) must stay on
	// the thread that execs the user command.
	runtime.LockOSThread()
	uid, err := applyIdentity(cfg)
	if err != nil {
		return report(err)
	}
	if err := applySecurity(cfg, uid); err != nil {
		return report(err)
	}

	if err := handshake(sync); err != nil {
		return report(err)
	}
	return execUser(cfg)
}

// setupRootfs prepares the container root: private mount namespace, the rootfs
// bind mount, /proc, /dev, /sys, volumes, masked paths and pivot_root.
func setupRootfs(cfg *Config) error {
	root := cfg.Rootfs.Merged
	if root == "" {
		return fmt.Errorf("container config has no rootfs path")
	}
	if err := makePrivate(); err != nil {
		return err
	}
	if err := bindMount(root, root, true); err != nil {
		return err
	}
	if err := writeContainerFiles(cfg); err != nil {
		return err
	}
	for _, m := range cfg.Mounts {
		if err := setupVolume(root, m); err != nil {
			return err
		}
	}
	if err := setupProc(root); err != nil {
		return err
	}
	if err := setupDev(root); err != nil {
		return err
	}
	if err := setupSys(root); err != nil {
		return err
	}
	masked := cfg.Security.MaskedPaths
	if masked == nil {
		masked = DefaultMaskedPaths
	}
	for _, p := range masked {
		if err := maskPath(root, p); err != nil {
			return err
		}
	}
	readonly := cfg.Security.ReadonlyPaths
	if readonly == nil {
		readonly = DefaultReadonlyPaths
	}
	for _, p := range readonly {
		if err := mountReadOnlyPath(root, p); err != nil {
			return err
		}
	}
	if cfg.Init {
		if err := bindSelfIntoRoot(root); err != nil {
			return err
		}
	}
	if cfg.ReadOnly {
		if err := remountReadOnly(root); err != nil {
			return err
		}
	}
	return pivot(root)
}

// bindSelfIntoRoot exposes the minibox binary inside the container so that
// --init can run the built-in reaper.
func bindSelfIntoRoot(root string) error {
	self := os.Getenv(EnvSelfBinary)
	if self == "" {
		self = "/proc/self/exe"
	}
	target := filepath.Join(root, "dev/.minibox-init")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if f, err := os.Create(target); err == nil {
		f.Close()
	}
	if err := bindMount(self, target, false); err != nil {
		return fmt.Errorf("install the built-in init: %w", err)
	}
	return nil
}

// writeContainerFiles writes /etc/resolv.conf and /etc/hosts from the resolved
// network configuration, before the rootfs can be made read-only.
func writeContainerFiles(cfg *Config) error {
	if cfg.Network.Mode == "none" && len(cfg.Network.DNS) == 0 {
		return nil
	}
	etc := filepath.Join(cfg.Rootfs.Merged, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	var resolv strings.Builder
	for _, ns := range cfg.Network.DNS {
		resolv.WriteString("nameserver " + ns + "\n")
	}
	if resolv.Len() > 0 {
		if err := replaceFile(filepath.Join(etc, "resolv.conf"), []byte(resolv.String()), 0o644); err != nil {
			return err
		}
	}
	hostname := cfg.Network.Hostname
	if hostname == "" {
		hostname = cfg.Hostname
	}
	var hosts strings.Builder
	hosts.WriteString("127.0.0.1\tlocalhost\n")
	hosts.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	if cfg.Network.ContainerIP != "" {
		hosts.WriteString(cfg.Network.ContainerIP + "\t" + hostname + "\n")
	}
	return replaceFile(filepath.Join(etc, "hosts"), []byte(hosts.String()), 0o644)
}

// replaceFile writes content to path, replacing symlinks and ignoring
// read-only filesystems.
func replaceFile(path string, content []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace %s: %w", path, err)
		}
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		if errorsIsReadOnly(err) {
			return nil
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// applyIdentity switches to the configured user and group and returns the
// resolved uid (-1 when no user was requested), which the security stage needs
// because setuid decides whether capabilities can still be installed.
func applyIdentity(cfg *Config) (int, error) {
	if cfg.User == "" {
		return -1, nil
	}
	spec, err := ParseUserSpec(cfg.User)
	if err != nil {
		return -1, err
	}
	if err := spec.Lookup("/"); err != nil {
		return -1, err
	}
	if spec.HasGID {
		if err := unix.Setgroups([]int{spec.GID}); err != nil && err != unix.EPERM {
			return -1, fmt.Errorf("set supplementary groups: %w", err)
		}
		if err := unix.Setgid(spec.GID); err != nil {
			return -1, fmt.Errorf("set gid to %d: %w", spec.GID, err)
		}
	}
	if spec.HasUID {
		if err := unix.Setuid(spec.UID); err != nil {
			return -1, fmt.Errorf("set uid to %d: %w", spec.UID, err)
		}
	}
	return spec.UID, nil
}

// applySecurity installs the capability set and no_new_privs, and validates the
// requested seccomp profile. The filter itself is installed by execUser as the
// last step before execve, so that a filter fault can never be mistaken for a
// setup failure and no runtime syscall happens after it is in force.
func applySecurity(cfg *Config, uid int) error {
	caps, err := security.ParseCapabilities(cfg.Security.Capabilities)
	if err != nil {
		return err
	}
	// setuid clears the permitted and effective capability sets, and without
	// CAP_SETPCAP in the effective set the kernel refuses to add them back, so
	// a non-root --user runs with no capabilities. Docker behaves the same way;
	// the alternative is retaining capabilities across the uid change with
	// PR_SET_KEEPCAPS, which would hand the user capabilities Docker does not.
	if uid > 0 {
		if len(cfg.Security.CapAdd) > 0 {
			fmt.Fprintf(os.Stderr, "minibox: warning: --cap-add is ignored for the non-root user %q; "+
				"capabilities are only installed for uid 0\n", cfg.User)
		}
		if cfg.Security.NoNewPrivs {
			return security.SetNoNewPrivs()
		}
		return nil
	}
	unavailable, err := security.ApplyCapabilities(caps)
	if err != nil {
		return fmt.Errorf("apply capabilities: %w", err)
	}
	if len(unavailable) > 0 {
		// Restricted hosts (Railway, Kubernetes restricted profiles, gVisor,
		// nested containers) do not provide the full Docker-like set; the
		// container still runs, minus those capabilities.
		word := "it"
		if len(unavailable) > 1 {
			word = "them"
		}
		fmt.Fprintf(os.Stderr, "minibox: warning: this host's bounding set lacks %s; the container runs without %s\n",
			strings.Join(unavailable, ", "), word)
	}
	if cfg.Security.NoNewPrivs {
		if err := security.SetNoNewPrivs(); err != nil {
			return err
		}
	}
	switch cfg.Security.Seccomp {
	case "unconfined", "", "default":
		return nil
	default:
		return fmt.Errorf("unknown seccomp profile %q", cfg.Security.Seccomp)
	}
}

// applySeccompFilter installs the configured seccomp profile on the calling
// thread. It runs immediately before execve.
func applySeccompFilter(cfg *Config) error {
	switch cfg.Security.Seccomp {
	case "unconfined", "":
		return nil
	default:
		filter, err := security.BuildFilter(security.DefaultProfile())
		if err != nil {
			return fmt.Errorf("build seccomp filter: %w", err)
		}
		if err := security.ApplySeccomp(filter); err != nil {
			return fmt.Errorf("apply seccomp filter: %w", err)
		}
		return nil
	}
}

// handshake tells the supervisor that setup is complete and waits for the go
// signal, which arrives once networking and cgroup limits are in place.
func handshake(sync *os.File) error {
	if _, err := fmt.Fprintf(sync, "%s %d\n", readyToken, os.Getpid()); err != nil {
		return fmt.Errorf("report readiness to the supervisor: %w", err)
	}
	reader := bufio.NewReader(sync)
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("wait for the supervisor: %w", err)
	}
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, goToken):
		return nil
	case strings.HasPrefix(line, errToken):
		return fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(line, errToken)))
	default:
		return fmt.Errorf("unexpected supervisor message %q", line)
	}
}

// execUser replaces the init process with the container command.
func execUser(cfg *Config) error {
	args := cfg.Args
	if len(args) == 0 {
		return fmt.Errorf("no command to run: the image has no entrypoint and no command was given")
	}
	if cfg.Cwd != "" {
		if err := os.MkdirAll(cfg.Cwd, 0o755); err != nil && !os.IsExist(err) {
			// A missing working directory is not fatal when it cannot be
			// created (read-only rootfs); fall back to /.
			cfg.Cwd = "/"
		}
		if err := os.Chdir(cfg.Cwd); err != nil {
			return fmt.Errorf("chdir to %s: %w", cfg.Cwd, err)
		}
	}
	env := cfg.Env
	if len(env) == 0 {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	path := lookupPath(args[0], env)
	if path == "" {
		return fmt.Errorf("exec %q: no such file in the container's PATH", args[0])
	}
	if cfg.Init {
		// The reaper runs the command as a child so that it can forward
		// signals and reap zombies.
		reaperArgs := append([]string{"minibox-init"}, args...)
		if err := applySeccompFilter(cfg); err != nil {
			return err
		}
		if err := syscall.Exec("/dev/.minibox-init", reaperArgs, env); err != nil {
			return fmt.Errorf("exec the built-in init: %w", err)
		}
	}
	if err := applySeccompFilter(cfg); err != nil {
		return err
	}
	if err := syscall.Exec(path, args, env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}

// lookupPath resolves a command name against the container environment's PATH.
func lookupPath(name string, env []string) string {
	if strings.ContainsRune(name, '/') {
		if _, err := os.Stat(name); err != nil {
			return ""
		}
		return name
	}
	pathEnv := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathEnv = strings.TrimPrefix(kv, "PATH=")
		}
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

// errorsIsReadOnly reports whether an error came from a read-only filesystem.
func errorsIsReadOnly(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "read-only file system") ||
		strings.Contains(err.Error(), "permission denied"))
}
