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

	Rootfs    Rootfs    `json:"rootfs"`
	Mounts    []Mount   `json:"mounts,omitempty"`
	Network   Network   `json:"network"`
	Resources Resources `json:"resources,omitempty"`
	Security  Security  `json:"security"`
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
