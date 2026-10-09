//go:build linux

// Package runtime describes a container's resolved runtime configuration and
// implements the re-exec/init machinery that turns that configuration into a
// running process.
package runtime

import "time"

// Config is the resolved runtime configuration for one container. It is written
// to containers/<id>/config.json and read back by the init stage, so it must
// stay JSON-serialisable and free of Go-only state.
type Config struct {
	ID         string    `json:"id"`
	Image      string    `json:"image"`
	Args       []string  `json:"args"`
	Env        []string  `json:"env"`
	Cwd        string    `json:"cwd"`
	User       string    `json:"user,omitempty"`
	Hostname   string    `json:"hostname"`
	Tty        bool      `json:"tty,omitempty"`
	Stdin      bool      `json:"stdin,omitempty"`
	Detach     bool      `json:"detach,omitempty"`
	AutoRemove bool      `json:"auto_remove,omitempty"`
	ReadOnly   bool      `json:"read_only,omitempty"`
	Init       bool      `json:"init,omitempty"`
	Created    time.Time `json:"created"`
	// Restart is the restart policy: no, on-failure[:max] or always.
	// Foreground runs ignore it (the CLI returns when the container exits);
	// detached containers are restarted by the shim.
	Restart string `json:"restart,omitempty"`
	// HealthCheck probes the container while it runs. Nil disables probing.
	HealthCheck *HealthCheck `json:"healthcheck,omitempty"`

	Rootfs    Rootfs    `json:"rootfs"`
	Mounts    []Mount   `json:"mounts,omitempty"`
	Network   Network   `json:"network"`
	Resources Resources `json:"resources,omitempty"`
	Security  Security  `json:"security"`
}

// HealthCheck describes a container health probe, mirroring the compose and
// Docker healthcheck options.
type HealthCheck struct {
	// Test is ["CMD", argv...] or ["CMD-SHELL", script].
	Test []string `json:"test"`
	// Interval between probes (default 30s).
	Interval time.Duration `json:"interval,omitempty"`
	// Timeout for a single probe (default 30s).
	Timeout time.Duration `json:"timeout,omitempty"`
	// Retries before the container is reported unhealthy (default 3).
	Retries int `json:"retries,omitempty"`
	// StartPeriod grace period before failures count (default 0).
	StartPeriod time.Duration `json:"start_period,omitempty"`
}

// Defaults for a health check that leaves fields unset.
const (
	DefaultHealthInterval = 30 * time.Second
	DefaultHealthTimeout  = 30 * time.Second
	DefaultHealthRetries  = 3
)

// Normalize fills in the defaults for unset health check fields.
func (h *HealthCheck) Normalize() {
	if h == nil {
		return
	}
	if h.Interval <= 0 {
		h.Interval = DefaultHealthInterval
	}
	if h.Timeout <= 0 {
		h.Timeout = DefaultHealthTimeout
	}
	if h.Retries <= 0 {
		h.Retries = DefaultHealthRetries
	}
}

// Command returns the argv to execute inside the container for one probe.
func (h *HealthCheck) Command() ([]string, error) {
	if h == nil || len(h.Test) == 0 {
		return nil, nil
	}
	switch h.Test[0] {
	case "NONE":
		return nil, nil
	case "CMD":
		if len(h.Test) < 2 {
			return nil, nil
		}
		return h.Test[1:], nil
	case "CMD-SHELL":
		if len(h.Test) < 2 {
			return nil, nil
		}
		return []string{"/bin/sh", "-c", h.Test[1]}, nil
	default:
		// A bare string is a shell command, as compose treats it.
		return []string{"/bin/sh", "-c", h.Test[0]}, nil
	}
}

// Rootfs describes where the container's root filesystem comes from.
type Rootfs struct {
	// Merged is the directory mounted over by overlayfs (or the plain rootfs
	// directory for single-layer images).
	Merged string `json:"merged"`
	// Lower holds the read-only layer directories, in overlayfs order (the
	// first entry is the top-most lower layer).
	Lower []string `json:"lower,omitempty"`
	// Upper and Work are the overlayfs writable layer directories.
	Upper string `json:"upper,omitempty"`
	Work  string `json:"work,omitempty"`
	// Overlay reports whether overlayfs should be used.
	Overlay bool `json:"overlay"`
}

// Mount is a mount that must be present inside the container.
type Mount struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"` // bind, tmpfs, proc, sysfs, devpts
	Options     []string `json:"options,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
}

// Network describes the container's network configuration.
type Network struct {
	// ContainerID is the container this network configuration belongs to.
	ContainerID string `json:"container_id,omitempty"`
	// Mode is one of none, host, bridge or pasta.
	Mode string `json:"mode"`
	// Hostname is the container hostname (also used for /etc/hosts).
	Hostname string `json:"hostname,omitempty"`
	// NetnsPath is the network namespace file the parent created, if any.
	NetnsPath string `json:"netns_path,omitempty"`
	// ContainerIP and Gateway are the addresses configured in bridge mode.
	ContainerIP string `json:"container_ip,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	// MergedRootfs is the container root directory the network layer may write
	// resolv.conf and hosts into; it is not serialised.
	MergedRootfs string `json:"-"`
	// DNS lists the nameservers written into the container's resolv.conf.
	DNS []string `json:"dns,omitempty"`
	// Ports lists published ports.
	Ports []PortMapping `json:"ports,omitempty"`
}

// PortMapping publishes a container port on the host.
type PortMapping struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"` // tcp or udp
}

// Resources mirrors the cgroup v2 limits applied to the container.
type Resources struct {
	MemoryBytes     int64  `json:"memory_bytes,omitempty"`
	MemorySwapBytes int64  `json:"memory_swap_bytes,omitempty"`
	CPUs            string `json:"cpus,omitempty"`
	PidsLimit       int64  `json:"pids_limit,omitempty"`
}

// Security holds the hardening options applied inside the container.
type Security struct {
	// Capabilities is the resolved capability list. Nil means the defaults.
	Capabilities []string `json:"capabilities,omitempty"`
	CapAdd       []string `json:"cap_add,omitempty"`
	CapDrop      []string `json:"cap_drop,omitempty"`
	// Seccomp is "default" or "unconfined".
	Seccomp string `json:"seccomp,omitempty"`
	// NoNewPrivs defaults to true.
	NoNewPrivs bool `json:"no_new_privs"`
	// MaskedPaths and ReadonlyPaths override the built-in lists when set.
	MaskedPaths   []string `json:"masked_paths,omitempty"`
	ReadonlyPaths []string `json:"readonly_paths,omitempty"`
	// Rootless reports whether the container runs in a user namespace.
	Rootless bool `json:"rootless,omitempty"`
}

// DefaultMaskedPaths are hidden from the container, following Docker.
var DefaultMaskedPaths = []string{
	"/proc/acpi",
	"/proc/asound",
	"/proc/kcore",
	"/proc/keys",
	"/proc/latency_stats",
	"/proc/timer_list",
	"/proc/timer_stats",
	"/proc/sched_debug",
	"/sys/firmware",
	"/proc/scsi",
}

// DefaultReadonlyPaths are mounted read-only inside the container.
var DefaultReadonlyPaths = []string{
	"/proc/bus",
	"/proc/fs",
	"/proc/irq",
	"/proc/sys",
	"/proc/sysrq-trigger",
}
