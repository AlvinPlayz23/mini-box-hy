//go:build linux

// Package network implements container networking: none, host, bridge (with
// veth pairs, NAT and port publishing) and pasta for rootless containers.
package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Mode is a network mode.
type Mode string

// Supported network modes.
const (
	ModeNone   Mode = "none"
	ModeHost   Mode = "host"
	ModeBridge Mode = "bridge"
	ModePasta  Mode = "pasta"
)

// DefaultBridge is the bridge interface minibox creates when rootful.
const DefaultBridge = "minibox0"

// DefaultSubnet is the address range used for containers.
const DefaultSubnet = "10.88.0.0/16"

// DefaultGateway is the bridge address inside DefaultSubnet.
const DefaultGateway = "10.88.0.1"

// ParseMode validates a --network value.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeNone, ModeHost, ModeBridge, ModePasta:
		return Mode(s), nil
	case "":
		return ModeNone, nil
	default:
		return "", fmt.Errorf("unknown network mode %q: use none, host, bridge or pasta", s)
	}
}

// PortMapping publishes a container port on the host.
type PortMapping struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string // tcp or udp
}

// ParsePortMapping parses docker-style -p values: "8080:80",
// "127.0.0.1:8080:80", "8080:80/udp" or "80" (random host port).
func ParsePortMapping(s string) (PortMapping, error) {
	m := PortMapping{Protocol: "tcp"}
	spec := s
	if i := strings.LastIndexByte(spec, '/'); i >= 0 {
		proto := strings.ToLower(spec[i+1:])
		if proto != "tcp" && proto != "udp" {
			return m, fmt.Errorf("invalid protocol %q in port mapping %q: use tcp or udp", proto, s)
		}
		m.Protocol = proto
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		port, err := parsePort(parts[0])
		if err != nil {
			return m, fmt.Errorf("invalid port mapping %q: %w", s, err)
		}
		m.HostPort = 0
		m.ContainerPort = port
	case 2:
		host, err := parsePort(parts[0])
		if err != nil {
			return m, fmt.Errorf("invalid host port in %q: %w", s, err)
		}
		cont, err := parsePort(parts[1])
		if err != nil {
			return m, fmt.Errorf("invalid container port in %q: %w", s, err)
		}
		m.HostPort, m.ContainerPort = host, cont
	case 3:
		m.HostIP = parts[0]
		host, err := parsePort(parts[1])
		if err != nil {
			return m, fmt.Errorf("invalid host port in %q: %w", s, err)
		}
		cont, err := parsePort(parts[2])
		if err != nil {
			return m, fmt.Errorf("invalid container port in %q: %w", s, err)
		}
		m.HostPort, m.ContainerPort = host, cont
	default:
		return m, fmt.Errorf("invalid port mapping %q: expected [HOST_IP:]HOST_PORT:CONTAINER_PORT[/proto]", s)
	}
	if m.ContainerPort == 0 {
		return m, fmt.Errorf("invalid port mapping %q: container port must not be 0", s)
	}
	return m, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("port %q is not a number between 0 and 65535", s)
	}
	return n, nil
}

// Config describes the network setup for one container.
type Config struct {
	ContainerID string
	// PID is the container init process; its network namespace lives at
	// /proc/<PID>/ns/net.
	PID  int
	Mode Mode
	// Hostname is written into /etc/hosts.
	Hostname string
	// Ports lists published ports.
	Ports []PortMapping
	// RootfsMerged is the container root, for resolv.conf and hosts.
	RootfsMerged string
	// StateDir is the container's state directory.
	StateDir string
	// Rootless selects pasta-style networking.
	Rootless bool
	// Bridge overrides the bridge interface name.
	Bridge string
	// Subnet overrides the bridge subnet (CIDR).
	Subnet string
	// DNS overrides the resolv.conf contents.
	DNS []string
}

// BridgeName returns the bridge interface for cfg.
func BridgeName(cfg Config) string {
	if cfg.Bridge != "" {
		return cfg.Bridge
	}
	return DefaultBridge
}

// Subnet returns the bridge subnet for cfg.
func Subnet(cfg Config) string {
	if cfg.Subnet != "" {
		return cfg.Subnet
	}
	return DefaultSubnet
}

// Setup prepares networking for a container whose init process is blocked
// waiting for the supervisor. The returned cleanup function is idempotent.
func Setup(cfg Config) (cleanup func() error, err error) {
	// resolv.conf and hosts are written by the container init from the config;
	// here we only add the container's own address when networking knows it.
	switch cfg.Mode {
	case ModeNone, "":
		return func() error { return nil }, nil
	case ModeHost:
		return func() error { return nil }, nil
	case ModeBridge:
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf("bridge networking needs root: run as root or use --network pasta")
		}
		return setupBridge(cfg)
	case ModePasta:
		return setupPasta(cfg)
	default:
		return nil, fmt.Errorf("unknown network mode %q", cfg.Mode)
	}
}

// Teardown removes everything Setup created for a container, best effort.
func Teardown(cfg Config, stateDir string) error {
	if stateDir != "" {
		cfg.StateDir = stateDir
	}
	switch cfg.Mode {
	case ModeBridge:
		return teardownBridge(cfg)
	case ModePasta:
		return teardownPasta(cfg)
	default:
		return nil
	}
}

// HostDNS returns the host's nameservers, falling back to public resolvers.
func HostDNS() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				out = append(out, fields[1])
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1", "8.8.8.8"}
}

// interfacesUp reports whether an interface exists and is up (used by tests and
// diagnostics).
func interfaceUp(name string) bool {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return false
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return false
	}
	return ifr.Uint16()&unix.IFF_UP != 0
}

// statePath returns a file inside the container's state directory.
func statePath(cfg Config, name string) string {
	if cfg.StateDir == "" {
		return filepath.Join(os.TempDir(), "minibox-"+cfg.ContainerID+"-"+name)
	}
	return filepath.Join(cfg.StateDir, name)
}
