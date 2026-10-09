//go:build linux

package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Environment variables passed to the re-executed init process.
const (
	EnvConfig      = "MINIBOX_CONFIG"
	EnvSyncFD      = "MINIBOX_SYNC_FD"
	EnvCgroupProcs = "MINIBOX_CGROUP_PROCS"
	EnvSelfBinary  = "MINIBOX_SELF_BINARY"
	syncPipeFD     = 3
	readyToken     = "READY"
	goToken        = "GO"
	errToken       = "ERR"
)

// StartOptions controls how the container init process is created.
type StartOptions struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File

	// CgroupFD, when >= 0, starts the child directly in its cgroup with
	// CLONE_INTO_CGROUP.
	CgroupFD int

	// HostNetwork shares the host network namespace.
	HostNetwork bool
	// Detach puts the child in its own session (used by the shim).
	Detach bool
	// Rootless creates a user namespace.
	Rootless bool
	// SingleIDMapping maps container root to the invoking user with the
	// kernel's implicit single-id mapping.
	SingleIDMapping bool
	// MultiIDMappings are applied with newuidmap/newgidmap after the fork.
	MultiIDMappings bool
	SubUIDs         SubIDRange
	SubGIDs         SubIDRange
	// CgroupProcs is written by init as a fallback when the kernel does not
	// support CLONE_INTO_CGROUP.
	CgroupProcs string
	// ExtraEnv is appended to the init process environment.
	ExtraEnv []string
}

// InitProcess is a started (but not yet released) container init process.
type InitProcess struct {
	Cmd  *exec.Cmd
	Sync *os.File

	reader *bufio.Reader
	pid    int
}

// StartInit re-executes the minibox binary as the container init process. The
// child blocks until Go or Fail is called, which lets the parent finish
// networking and cgroup setup first.
func StartInit(configPath string, opts StartOptions) (*InitProcess, error) {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create init sync socket: %w", err)
	}
	parentFile := os.NewFile(uintptr(pair[0]), "sync-parent")
	childFile := os.NewFile(uintptr(pair[1]), "sync-child")
	// The child end must survive exec, so clear CLOEXEC on it.
	if err := unix.SetNonblock(int(childFile.Fd()), false); err != nil {
		// Non-blocking sockets still work for read/write; ignore.
		_ = err
	}
	if _, err := unix.FcntlInt(childFile.Fd(), unix.F_SETFD, 0); err != nil {
		parentFile.Close()
		childFile.Close()
		return nil, fmt.Errorf("prepare init sync socket: %w", err)
	}

	cmd := exec.Command(self, "init")
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	cmd.ExtraFiles = []*os.File{childFile}
	env := []string{
		EnvConfig + "=" + configPath,
		EnvSyncFD + "=" + strconv.Itoa(syncPipeFD),
		EnvSelfBinary + "=" + self,
	}
	if opts.CgroupProcs != "" {
		env = append(env, EnvCgroupProcs+"="+opts.CgroupProcs)
	}
	env = append(env, opts.ExtraEnv...)
	cmd.Env = env

	attr := &syscall.SysProcAttr{
		Cloneflags: cloneFlags(opts),
		Pdeathsig:  syscall.SIGKILL,
	}
	if opts.Detach {
		attr.Setsid = true
	}
	if opts.CgroupFD >= 0 {
		attr.UseCgroupFD = true
		attr.CgroupFD = opts.CgroupFD
	}
	if opts.Rootless && opts.SingleIDMapping {
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr = attr

	if err := cmd.Start(); err != nil {
		parentFile.Close()
		childFile.Close()
		return nil, fmt.Errorf("start container init: %w", err)
	}
	childFile.Close()
	p := &InitProcess{
		Cmd:    cmd,
		Sync:   parentFile,
		reader: bufio.NewReader(parentFile),
		pid:    cmd.Process.Pid,
	}
	if opts.Rootless && opts.MultiIDMappings {
		if err := WriteMappings(p.pid, opts.SubUIDs, opts.SubGIDs); err != nil {
			p.Kill()
			return nil, fmt.Errorf("set up user namespace mappings: %w", err)
		}
	}
	return p, nil
}

// cloneFlags builds the namespace flags for the init process.
func cloneFlags(opts StartOptions) uintptr {
	flags := uintptr(unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC)
	if !opts.HostNetwork {
		flags |= unix.CLONE_NEWNET
	}
	if opts.Rootless {
		flags |= unix.CLONE_NEWUSER
	}
	return flags
}

// PID returns the init process pid as seen from the host.
func (p *InitProcess) PID() int { return p.pid }

// WaitReady blocks until the init process reports that its filesystem and
// security setup is complete.
func (p *InitProcess) WaitReady(timeout time.Duration) error {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := p.reader.ReadString('\n')
		ch <- result{line: strings.TrimSpace(line), err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("container init exited during setup: %w", res.err)
		}
		if strings.HasPrefix(res.line, readyToken) {
			return nil
		}
		if strings.HasPrefix(res.line, errToken) {
			return fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(res.line, errToken)))
		}
		return fmt.Errorf("unexpected message from container init: %q", res.line)
	case <-time.After(timeout):
		return fmt.Errorf("container init did not become ready within %s", timeout)
	}
}

// Go releases the init process so it can exec the user command.
func (p *InitProcess) Go() error {
	if _, err := p.Sync.Write([]byte(goToken + "\n")); err != nil {
		return fmt.Errorf("release container init: %w", err)
	}
	return nil
}

// Fail tells the init process to give up with a message.
func (p *InitProcess) Fail(reason string) error {
	_, err := p.Sync.Write([]byte(errToken + " " + reason + "\n"))
	return err
}

// Close releases the sync socket.
func (p *InitProcess) Close() error {
	if p.Sync == nil {
		return nil
	}
	err := p.Sync.Close()
	p.Sync = nil
	return err
}

// Kill terminates the init process and any container processes.
func (p *InitProcess) Kill() {
	if p.Cmd != nil && p.Cmd.Process != nil {
		_ = p.Cmd.Process.Kill()
	}
	p.Close()
}

// Wait waits for the init process to exit and returns its exit code.
func (p *InitProcess) Wait() (int, error) {
	if p.Cmd == nil {
		return 0, fmt.Errorf("init process was never started")
	}
	err := p.Cmd.Wait()
	p.Close()
	return ExitCode(err)
}

// ExitCode extracts a process exit code, mapping signals to 128+signal.
func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return status.ExitStatus(), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
