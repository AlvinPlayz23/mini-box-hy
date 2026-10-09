//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Reap runs argv as a child process as PID 1 inside the container: it forwards
// signals, reaps zombies and exits with the child's status. It returns the exit
// code for main to use.
func Reap(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "minibox-init: no command given")
		return 127
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "minibox-init: start %s: %v\n", argv[0], err)
		return 127
	}
	child := cmd.Process.Pid

	forward := make(chan os.Signal, 16)
	signal.Notify(forward, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGCONT, syscall.SIGTSTP)
	children := make(chan os.Signal, 16)
	signal.Notify(children, syscall.SIGCHLD)

	done := make(chan int, 1)
	go func() {
		code, _ := ExitCode(cmd.Wait())
		done <- code
	}()

	for {
		select {
		case sig := <-forward:
			if s, ok := sig.(syscall.Signal); ok {
				_ = unix.Kill(child, s)
			}
		case <-children:
			reapZombies()
		case code := <-done:
			reapZombies()
			return code
		}
	}
}

// reapZombies collects any exited children without blocking.
func reapZombies() {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}
