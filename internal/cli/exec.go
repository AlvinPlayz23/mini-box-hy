//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/security"
	"golang.org/x/sys/unix"
)

// execOptions collects `minibox exec` flags.
type execOptions struct {
	interactive bool
	tty         bool
	env         stringList
	user        string
	workdir     string
	container   string
	command     []string
}

func parseExecFlags(args []string) (*execOptions, error) {
	o := &execOptions{}
	fs := newFlagSet("exec")
	fs.BoolVar(&o.interactive, "i", false, "")
	fs.BoolVar(&o.tty, "t", false, "")
	fs.Var(&o.env, "e", "")
	fs.StringVar(&o.user, "u", "", "")
	fs.StringVar(&o.workdir, "w", "", "")
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return nil, usageError("usage: minibox exec [-it] CONTAINER CMD...")
	}
	o.container = rest[0]
	o.command = rest[1:]
	for _, a := range args {
		if a == "-it" || a == "--it" {
			o.tty, o.interactive = true, true
		}
	}
	return o, nil
}

// execCommand runs a command in a running container by joining its namespaces
// from a re-executed helper.
func execCommand(env *Env, args []string) int {
	opts, err := parseExecFlags(args)
	if err != nil {
		return reportError(env, err)
	}
	c, err := container.Load(env.Store, opts.container)
	if err != nil {
		return reportError(env, err)
	}
	if c.State.Status != container.StatusRunning || c.State.PID <= 0 {
		return reportError(env, fmt.Errorf("container %s is not running: start it or use `minibox ps -a`", c.ID[:12]))
	}
	if os.Geteuid() != 0 {
		return reportError(env, fmt.Errorf("exec is only supported for rootful containers in this version; "+
			"rootless exec needs user-namespace joining, which Go processes cannot do safely (see docs/SECURITY.md)"))
	}

	self, err := os.Executable()
	if err != nil {
		return reportError(env, err)
	}
	cmdArgs := append([]string{"exec-child",
		"--pid", strconv.Itoa(c.State.PID),
		"--config", c.ConfigPath(),
	}, opts.envFlags()...)
	cmdArgs = append(cmdArgs, "--")
	cmdArgs = append(cmdArgs, opts.command...)

	cmd := exec.Command(self, cmdArgs...)
	cmd.Env = os.Environ()
	if opts.tty {
		pty, err := container.OpenPTY()
		if err != nil {
			return reportError(env, err)
		}
		defer pty.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = pty.Slave, pty.Slave, pty.Slave
		if term, terr := container.MakeRaw(env.Stdin); terr == nil {
			defer term.Restore()
		}
		rows, cols := container.TerminalSize(env.Stdin)
		_ = pty.SetWinsize(rows, cols)
		go container.CopyPTY(pty.Master, env.Stdin, env.StdoutFile)
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.StdoutFile, env.StderrFile
	}
	if err := cmd.Run(); err != nil {
		code, _ := runtime.ExitCode(err)
		return code
	}
	return 0
}

// envFlags renders the -e and -w flags for the child.
func (o *execOptions) envFlags() []string {
	var out []string
	for _, kv := range o.env {
		out = append(out, "-e", kv)
	}
	if o.user != "" {
		out = append(out, "-u", o.user)
	}
	if o.workdir != "" {
		out = append(out, "-w", o.workdir)
	}
	return out
}

// execChildCommand is the hidden helper that joins a container's namespaces and
// runs a command there.
func execChildCommand(env *Env, args []string) int {
	fs := newFlagSet("exec-child")
	pid := fs.Int("pid", 0, "")
	configPath := fs.String("config", "", "")
	user := fs.String("u", "", "")
	workdir := fs.String("w", "", "")
	var extraEnv stringList
	fs.Var(&extraEnv, "e", "")
	if err := parseFlags(fs, args); err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: %v\n", err)
		return 126
	}
	command := fs.Args()
	if *pid == 0 || len(command) == 0 {
		fmt.Fprintln(os.Stderr, "minibox exec-child: --pid and a command are required")
		return 126
	}
	cfg, err := runtime.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: %v\n", err)
		return 126
	}
	if err := joinNamespaces(*pid, cfg.Security.Rootless); err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: %v\n", err)
		return 126
	}
	envList := mergeEnv(cfg.Env, extraEnv)
	dir := firstNonEmpty(*workdir, cfg.Cwd, "/")
	argv := command
	path, err := lookPathInContainer(argv[0], envList)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: %v\n", err)
		return 127
	}
	if *user == "" {
		*user = cfg.User
	}
	if err := applyExecSecurity(cfg, *user); err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: %v\n", err)
		return 126
	}
	attr := &syscall.ProcAttr{
		Dir:   dir,
		Env:   envList,
		Files: []uintptr{0, 1, 2},
	}
	childPID, err := syscall.ForkExec(path, argv, attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: exec %s: %v\n", path, err)
		return 126
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(childPID, &status, 0, nil); err != nil {
		fmt.Fprintf(os.Stderr, "minibox exec-child: wait: %v\n", err)
		return 126
	}
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

// joinNamespaces enters the namespaces of a container process.
func joinNamespaces(pid int, rootless bool) error {
	type ns struct {
		name string
		flag int
	}
	order := []ns{
		{"mnt", unix.CLONE_NEWNS},
		{"uts", unix.CLONE_NEWUTS},
		{"ipc", unix.CLONE_NEWIPC},
		{"net", unix.CLONE_NEWNET},
		{"pid", unix.CLONE_NEWPID},
	}
	if rootless {
		return fmt.Errorf("rootless exec is not supported yet")
	}
	// setns must happen on a thread that is never used for anything else.
	locked := make(chan struct{})
	go func() {
		runtimeLockOSThread()
		for _, n := range order {
			path := filepath.Join("/proc", strconv.Itoa(pid), "ns", n.name)
			fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err != nil {
				fmt.Fprintf(os.Stderr, "minibox exec-child: open %s: %v\n", path, err)
				os.Exit(126)
			}
			if err := unix.Setns(fd, n.flag); err != nil {
				unix.Close(fd)
				fmt.Fprintf(os.Stderr, "minibox exec-child: join %s namespace of pid %d: %v\n", n.name, pid, err)
				os.Exit(126)
			}
			unix.Close(fd)
		}
		close(locked)
	}()
	<-locked
	return nil
}

// applyExecSecurity applies the container's identity and security settings to
// the exec'd process.
func applyExecSecurity(cfg *runtime.Config, user string) error {
	uid := -1
	if user != "" {
		spec, err := runtime.ParseUserSpec(user)
		if err != nil {
			return err
		}
		if err := spec.Lookup("/"); err != nil {
			return err
		}
		if spec.HasGID {
			_ = unix.Setgroups([]int{spec.GID})
			if err := unix.Setgid(spec.GID); err != nil {
				return err
			}
		}
		if spec.HasUID {
			if err := unix.Setuid(spec.UID); err != nil {
				return err
			}
		}
		uid = spec.UID
	}
	// setuid cleared the capability sets and an unprivileged process cannot
	// re-add them, so a non-root --user gets no capabilities (as in Docker).
	// The seccomp filter below still applies.
	if uid <= 0 {
		caps, err := security.ParseCapabilities(cfg.Security.Capabilities)
		if err != nil {
			return err
		}
		if _, err := security.ApplyCapabilities(caps); err != nil {
			return err
		}
	}
	if cfg.Security.Seccomp == "default" {
		filter, err := security.BuildFilter(security.DefaultProfile())
		if err != nil {
			return err
		}
		return security.ApplySeccomp(filter)
	}
	return nil
}

// lookPathInContainer resolves a command inside the container root filesystem.
func lookPathInContainer(name string, env []string) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
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
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("exec %q: command not found in the container's PATH", name)
}
