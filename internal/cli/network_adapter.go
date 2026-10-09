//go:build linux

package cli

import (
	"os"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/network"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// netHook adapts the network package to the container package's hook interface.
type netHook struct{}

// networkHook returns the hook used by run and the shim.
func networkHook() container.NetworkHook { return netHook{} }

// Setup implements container.NetworkHook.
func (netHook) Setup(req container.NetworkRequest) (runtime.Network, func() error, error) {
	mode, err := network.ParseMode(req.Config.Mode)
	if err != nil {
		return req.Config, nil, err
	}
	cfg := network.Config{
		ContainerID:  req.ContainerID,
		PID:          req.PID,
		Mode:         mode,
		Hostname:     req.Config.Hostname,
		Ports:        toNetworkPorts(req.Config.Ports),
		RootfsMerged: req.Config.MergedRootfs,
		StateDir:     req.StateDir,
		Rootless:     os.Geteuid() != 0,
		DNS:          req.Config.DNS,
	}
	cleanup, err := network.Setup(cfg)
	if err != nil {
		return req.Config, nil, err
	}
	updated := req.Config
	updated.Mode = string(mode)
	if mode == network.ModeBridge {
		if ip, gerr := network.ReadContainerIP(req.StateDir); gerr == nil {
			updated.ContainerIP = ip
		}
		updated.Gateway = network.DefaultGateway
	}
	return updated, cleanup, nil
}

// Teardown implements container.NetworkHook.
func (netHook) Teardown(cfg runtime.Network, stateDir string) error {
	mode, err := network.ParseMode(cfg.Mode)
	if err != nil {
		return err
	}
	return network.Teardown(network.Config{
		ContainerID: cfg.ContainerID,
		Mode:        mode,
		Ports:       toNetworkPorts(cfg.Ports),
		StateDir:    stateDir,
	}, stateDir)
}

// toNetworkPorts converts runtime port mappings into network ones.
func toNetworkPorts(ports []runtime.PortMapping) []network.PortMapping {
	out := make([]network.PortMapping, 0, len(ports))
	for _, p := range ports {
		out = append(out, network.PortMapping{
			HostIP:        p.HostIP,
			HostPort:      p.HostPort,
			ContainerPort: p.ContainerPort,
			Protocol:      p.Protocol,
		})
	}
	return out
}

// netConfig carries the container network settings derived from run flags.
type netConfig struct {
	Mode  string
	Ports []runtime.PortMapping
}

func networkConfig(opts *runOptions) (netConfig, error) {
	out := netConfig{}
	mode := opts.network
	if mode == "" {
		if os.Geteuid() == 0 {
			mode = string(network.ModeBridge)
		} else {
			mode = string(network.ModePasta)
		}
	}
	parsed, err := network.ParseMode(mode)
	if err != nil {
		return out, err
	}
	out.Mode = string(parsed)
	for _, spec := range opts.publish {
		m, err := network.ParsePortMapping(spec)
		if err != nil {
			return out, err
		}
		out.Ports = append(out.Ports, runtime.PortMapping{
			HostIP:        m.HostIP,
			HostPort:      m.HostPort,
			ContainerPort: m.ContainerPort,
			Protocol:      m.Protocol,
		})
	}
	return out, nil
}

// hostDNS returns the resolvers to write into the container.
func hostDNS() []string { return network.HostDNS() }

// initNetworkWiring registers the teardown function the container package uses
// for Wait, prune and rm, so that the container package stays independent of
// the network package.
func initNetworkWiring() {
	container.SetNetworkTeardown(func(cfg runtime.Network, stateDir string) error {
		return netHook{}.Teardown(cfg, stateDir)
	})
}
