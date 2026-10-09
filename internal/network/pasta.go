//go:build linux

package network

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// pastaProcesses tracks running pasta helpers so teardown can stop them.
var pastaProcesses sync.Map

// setupPasta starts pasta for a container, which provides NAT, DNS forwarding
// and port publishing without root.
func setupPasta(cfg Config) (func() error, error) {
	path, err := exec.LookPath("pasta")
	if err != nil {
		return nil, fmt.Errorf("--network pasta needs the pasta binary from the passt package, which is not on PATH: " +
			"install it (for example `apt-get install -y passt`) or use --network host")
	}
	args := []string{"--config-net", "--quiet", "--foreground"}
	for _, p := range cfg.Ports {
		flag := "-t"
		if p.Protocol == "udp" {
			flag = "-u"
		}
		host := p.HostPort
		if host == 0 {
			host = p.ContainerPort
		}
		args = append(args, flag, strconv.Itoa(host)+":"+strconv.Itoa(p.ContainerPort))
	}
	args = append(args, strconv.Itoa(cfg.PID))

	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start pasta: %w", err)
	}
	pastaProcesses.Store(cfg.ContainerID, cmd)

	// Give pasta a moment to configure the namespace; the container stays
	// blocked until this returns.
	time.Sleep(100 * time.Millisecond)
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return nil, fmt.Errorf("pasta exited immediately: check `pasta --help` and that the container pid %d is alive", cfg.PID)
	}
	_ = WriteResolvConf(cfg.RootfsMerged, hostDNSOrDefault(cfg.DNS))
	_ = WriteHosts(cfg.RootfsMerged, cfg.Hostname, "")

	cleanup := func() error { return stopPasta(cfg.ContainerID) }
	return cleanup, nil
}

// teardownPasta stops a container's pasta helper.
func teardownPasta(cfg Config) error { return stopPasta(cfg.ContainerID) }

// stopPasta terminates the pasta process for a container.
func stopPasta(containerID string) error {
	value, ok := pastaProcesses.Load(containerID)
	if !ok {
		return nil
	}
	cmd, ok := value.(*exec.Cmd)
	if !ok || cmd.Process == nil {
		pastaProcesses.Delete(containerID)
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			break
		}
		if cmd.Process.Signal(syscall.Signal(0)) != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	pastaProcesses.Delete(containerID)
	return nil
}

// PastaCommandLine is a helper for diagnostics: it reports the pasta command
// line that would be used for a container.
func PastaCommandLine(cfg Config) string {
	var parts []string
	for _, p := range cfg.Ports {
		flag := "-t"
		if strings.EqualFold(p.Protocol, "udp") {
			flag = "-u"
		}
		parts = append(parts, flag, fmt.Sprintf("%d:%d", p.HostPort, p.ContainerPort))
	}
	return "pasta --config-net " + strings.Join(parts, " ") + " " + strconv.Itoa(cfg.PID)
}
