//go:build linux

package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// shimEnv names the container the shim supervises.
const shimEnv = "MINIBOX_SHIM_ID"

// startDetached forks the shim, which becomes the detached container's
// supervisor, and waits until it reports success.
func startDetached(env *Env, id string, cfg *runtime.Config) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the minibox binary: %w", err)
	}
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create shim pipe: %w", err)
	}
	defer pipeR.Close()

	cmd := exec.Command(self, "shim")
	cmd.Env = append(os.Environ(), shimEnv+"="+id)
	cmd.ExtraFiles = []*os.File{pipeW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		pipeW.Close()
		return fmt.Errorf("start the container shim: %w", err)
	}
	pipeW.Close()

	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(pipeR).ReadString('\n')
		ch <- result{line: strings.TrimSpace(line), err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return fmt.Errorf("the container shim exited during startup: %w", res.err)
		}
		switch {
		case strings.HasPrefix(res.line, "started"):
			return nil
		case strings.HasPrefix(res.line, "error"):
			return fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(res.line, "error")))
		default:
			return fmt.Errorf("unexpected shim message %q", res.line)
		}
	case <-time.After(60 * time.Second):
		return fmt.Errorf("container %s did not start within 60s", id[:12])
	}
}

// shimCommand is the hidden `minibox shim` entry point: it supervises a
// detached container, owns its log file and cleans up when it exits.
func shimCommand(env *Env, args []string) int {
	TuneShimRuntime()
	id := os.Getenv(shimEnv)
	if id == "" {
		fmt.Fprintln(os.Stderr, "minibox shim: MINIBOX_SHIM_ID is not set")
		return 1
	}
	report := os.NewFile(3, "shim-report")
	fail := func(err error) int {
		if report != nil {
			fmt.Fprintf(report, "error %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
			report.Close()
		}
		fmt.Fprintf(os.Stderr, "minibox shim: %v\n", err)
		return 1
	}

	c, err := container.Load(env.Store, id)
	if err != nil {
		return fail(err)
	}
	logFile, err := os.OpenFile(c.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open container log: %w", err))
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fail(err)
	}
	defer devNull.Close()

	env.Progress = func(msg string) { fmt.Fprintf(logFile, "minibox: %s\n", msg) }
	cfg, err := runtime.LoadConfig(c.ConfigPath())
	if err != nil {
		return fail(err)
	}

	restarts := 0
	for {
		startOpts := container.StartOptions{
			Stdin:    devNull,
			Stdout:   logFile,
			Stderr:   logFile,
			Network:  networkHook(),
			Progress: env.Progress,
			Detach:   true,
			Rootless: os.Geteuid() != 0,
		}
		started, err := container.Start(context.Background(), env.Store, id, startOpts)
		if err != nil {
			cleanupFailedStart(env, id)
			return fail(err)
		}
		if report != nil {
			fmt.Fprintf(report, "started %d\n", started.Init.PID())
			report.Close()
			report = nil
		}
		code, waitErr := started.Wait(context.Background())
		if waitErr != nil {
			fmt.Fprintf(logFile, "minibox: %v\n", waitErr)
		}
		if !container.ShouldRestart(cfg.Restart, 0, restarts, code) {
			return code
		}
		restarts++
		if live, err := container.Load(env.Store, id); err == nil {
			live.State.Restarts = restarts
			_ = live.SaveState()
		}
		fmt.Fprintf(logFile, "minibox: restarting (policy %s, restart %d) after exit code %d\n", cfg.Restart, restarts, code)
		time.Sleep(200 * time.Millisecond)
	}
}
