//go:build linux

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/security"
	"golang.org/x/sys/unix"
)

// doctorCommand implements `minibox doctor`: a read-only checklist of what this
// host can and cannot do, so a failure on an unfamiliar machine is diagnosable
// in one command.
func doctorCommand(env *Env, args []string) error {
	root := env.Store.Root
	warnings := 0
	failures := 0

	line := func(status, what, detail string) {
		switch status {
		case "fail":
			failures++
		case "warn":
			warnings++
		}
		fmt.Fprintf(env.Stdout, "%-5s %-34s %s\n", status, what, detail)
	}

	fmt.Fprintf(env.Stdout, "minibox doctor - host capability checklist\n\n")

	// Kernel and cgroups.
	var uname unix.Utsname
	if err := unix.Uname(&uname); err == nil {
		line("ok", "kernel", string(uname.Release[:bytesUntilNul(uname.Release[:])]))
	}
	if cgroup.Available() {
		if mgr, err := cgroup.Detect(); err == nil {
			if mgr.ControllersEnabled() {
				line("ok", "cgroup v2 limits", "memory, cpu and pids delegated")
			} else {
				line("warn", "cgroup v2 limits", "controllers not delegated: --memory/--cpus/--pids-limit will fail")
			}
		} else {
			line("warn", "cgroup v2", err.Error())
		}
	} else {
		line("fail", "cgroup v2", "missing /sys/fs/cgroup/cgroup.controllers: boot with systemd.unified_cgroup_hierarchy=1")
	}

	// Identity and capabilities.
	if os.Geteuid() == 0 {
		line("ok", "privileges", "running as root")
	} else {
		line("warn", "privileges", fmt.Sprintf("uid %d: bridge networking needs root, and rootless mode needs user namespaces", os.Geteuid()))
	}
	if requested, err := security.ResolveCapabilities(nil, nil); err == nil {
		_, unavailable := security.SplitGrantable(requested)
		if len(unavailable) == 0 {
			line("ok", "capabilities", fmt.Sprintf("all %d default capabilities available", len(requested)))
		} else {
			line("warn", "capabilities", fmt.Sprintf("host lacks %s; containers run without them", strings.Join(unavailable, ", ")))
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/seccomp/actions_avail"); err == nil {
		line("ok", "seccomp", strings.TrimSpace(string(data)))
	} else {
		line("warn", "seccomp", "no /proc/sys/kernel/seccomp/actions_avail: the default filter may not load")
	}
	if data, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		value := strings.TrimSpace(string(data))
		if value == "0" {
			line("warn", "user namespaces", "max_user_namespaces is 0: rootless mode is unavailable")
		} else {
			line("ok", "user namespaces", "max_user_namespaces = "+value)
		}
	} else {
		line("ok", "user namespaces", "no max_user_namespaces knob (older kernel)")
	}

	// Filesystems and files.
	if data, err := os.ReadFile("/proc/filesystems"); err == nil {
		if strings.Contains(string(data), "overlay") {
			line("ok", "overlayfs", "available")
		} else {
			line("fail", "overlayfs", "not in /proc/filesystems: image layers cannot be mounted")
		}
	}
	probe := root + "/.doctor-write-probe"
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		line("fail", "state root", fmt.Sprintf("%s is not writable: %v", root, err))
	} else {
		os.Remove(probe)
		line("ok", "state root", root)
	}

	// Optional helpers.
	for _, tool := range []struct {
		name, why string
		needed    bool
	}{
		{"nft", "NAT and -p port publishing in bridge mode", true},
		{"pasta", "rootless networking (--network pasta)", false},
		{"newuidmap", "multi-id rootless mappings from /etc/subuid", false},
		{"newgidmap", "multi-id rootless mappings from /etc/subgid", false},
	} {
		path, err := exec.LookPath(tool.name)
		switch {
		case err == nil:
			line("ok", tool.name, path)
		case tool.needed:
			line("warn", tool.name, "missing: "+tool.why+" (install the nftables package, or use --network host)")
		default:
			line("warn", tool.name, "missing: "+tool.why)
		}
	}

	fmt.Fprintf(env.Stdout, "\n%d failure(s), %d warning(s)\n", failures, warnings)
	if failures > 0 {
		return fmt.Errorf("this host cannot run containers until the failures above are fixed")
	}
	return nil
}

// bytesUntilNul trims a fixed-size C string from uname.
func bytesUntilNul(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
