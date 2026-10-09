//go:build linux

package cli

import (
	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/network"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/security"
)

// parseImageRef parses an image reference for compose services.
func parseImageRef(s string) (image.Reference, error) {
	return image.ParseReference(s)
}

// parsePortSpec converts a compose port string into a runtime port mapping.
func parsePortSpec(spec string) (runtime.PortMapping, error) {
	m, err := network.ParsePortMapping(spec)
	if err != nil {
		return runtime.PortMapping{}, err
	}
	return runtime.PortMapping{
		HostIP:        m.HostIP,
		HostPort:      m.HostPort,
		ContainerPort: m.ContainerPort,
		Protocol:      m.Protocol,
	}, nil
}

// parseBytesFlag parses a memory value such as "512m".
func parseBytesFlag(s string) (int64, error) { return cgroup.ParseBytes(s) }

// resolveCaps computes the capability set for a service.
func resolveCaps(add, drop []string) ([]string, error) {
	caps, err := security.ResolveCapabilities(add, drop)
	if err != nil {
		return nil, err
	}
	return security.CapabilityNames(caps), nil
}
