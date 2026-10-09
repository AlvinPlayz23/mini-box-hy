//go:build linux

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// linkName returns the host-side veth name for a container (limited to 15
// characters by the kernel).
func linkName(containerID string) (host, container string) {
	suffix := containerID
	if len(suffix) > 10 {
		suffix = suffix[:10]
	}
	return "mb" + suffix, "mb" + suffix + "c"
}

// setupBridge creates the bridge if needed, wires a veth pair into the
// container's network namespace and installs NAT plus port publishing.
func setupBridge(cfg Config) (func() error, error) {
	bridgeName := BridgeName(cfg)
	subnet := Subnet(cfg)
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("invalid bridge subnet %q: %w", subnet, err)
	}
	gateway := DefaultGateway
	if cfg.Subnet != "" {
		gw := offsetIP(ipnet, 1)
		if gw == nil {
			return nil, fmt.Errorf("cannot derive a gateway from subnet %q", subnet)
		}
		gateway = gw.String()
	}
	if err := ensureBridge(bridgeName, gateway, ipnet); err != nil {
		return nil, err
	}

	containerIP, err := AllocateIP(cfg.StateDir, cfg.ContainerID, subnet)
	if err != nil {
		return nil, err
	}
	hostIf, contIf := linkName(cfg.ContainerID)
	if err := createVethPair(hostIf, contIf, bridgeName, cfg.PID); err != nil {
		_ = ReleaseIP(cfg.StateDir, cfg.ContainerID)
		return nil, err
	}
	if err := configureContainerNetns(cfg.PID, contIf, containerIP, gateway, ipnet); err != nil {
		_ = deleteLink(hostIf)
		_ = ReleaseIP(cfg.StateDir, cfg.ContainerID)
		return nil, err
	}
	if err := addHostRoute(containerIP, hostIf); err != nil {
		_ = deleteLink(hostIf)
		_ = ReleaseIP(cfg.StateDir, cfg.ContainerID)
		return nil, err
	}

	var publishedPorts []PortMapping
	for _, p := range cfg.Ports {
		publishedPorts = append(publishedPorts, p)
	}
	if err := ensureNAT(bridgeName, subnet, cfg.ContainerID, containerIP, publishedPorts); err != nil {
		_ = deleteLink(hostIf)
		_ = ReleaseIP(cfg.StateDir, cfg.ContainerID)
		return nil, err
	}

	// Record the addresses on disk so that teardown and diagnostics work even
	// if the supervisor dies.
	_ = writeNetworkState(cfg, containerIP, gateway)
	_ = WriteResolvConf(cfg.RootfsMerged, hostDNSOrDefault(cfg.DNS))
	_ = WriteHosts(cfg.RootfsMerged, cfg.Hostname, containerIP)

	cleanup := func() error {
		var firstErr error
		record := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		record(removeNAT(cfg.ContainerID, publishedPorts))
		record(deleteLink(hostIf))
		record(ReleaseIP(cfg.StateDir, cfg.ContainerID))
		record(os.Remove(statePath(cfg, "network.json")))
		return firstErr
	}
	return cleanup, nil
}

// teardownBridge removes bridge resources for a container.
func teardownBridge(cfg Config) error {
	hostIf, _ := linkName(cfg.ContainerID)
	var firstErr error
	if err := removeNAT(cfg.ContainerID, cfg.Ports); err != nil && !isNftMissing(err) {
		firstErr = err
	}
	if err := deleteLink(hostIf); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ensureBridge creates the minibox bridge and assigns it the gateway address.
func ensureBridge(name, gateway string, ipnet *net.IPNet) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		attrs := netlink.NewLinkAttrs()
		attrs.Name = name
		bridge := &netlink.Bridge{LinkAttrs: attrs}
		if err := netlink.LinkAdd(bridge); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create bridge %s: %w\nhint: bridge networking needs CAP_NET_ADMIN (run as root)", name, err)
		}
		if link, err = netlink.LinkByName(name); err != nil {
			return fmt.Errorf("look up bridge %s after creating it: %w", name, err)
		}
	}
	addr, err := netlink.ParseAddr(gateway + "/" + prefixLen(ipnet))
	if err != nil {
		return fmt.Errorf("parse gateway address %s: %w", gateway, err)
	}
	existing, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	found := false
	for _, a := range existing {
		if a.IPNet != nil && a.IPNet.IP.Equal(addr.IPNet.IP) {
			found = true
			break
		}
	}
	if !found {
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("assign %s to bridge %s: %w", gateway, name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring bridge %s up: %w", name, err)
	}
	return nil
}

// prefixLen returns the CIDR prefix length of a network.
func prefixLen(ipnet *net.IPNet) string {
	ones, _ := ipnet.Mask.Size()
	return strconv.Itoa(ones)
}

// createVethPair creates the veth pair, attaches the host end to the bridge and
// moves the container end into the container's network namespace.
func createVethPair(hostIf, contIf, bridgeName string, pid int) error {
	if existing, err := netlink.LinkByName(hostIf); err == nil {
		// A leftover interface from a crashed run: remove it and start over.
		_ = netlink.LinkDel(existing)
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name = hostIf
	attrs.MTU = 1500
	veth := &netlink.Veth{LinkAttrs: attrs, PeerName: contIf}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth pair %s/%s: %w", hostIf, contIf, err)
	}
	hostLink, err := netlink.LinkByName(hostIf)
	if err != nil {
		return fmt.Errorf("look up %s: %w", hostIf, err)
	}
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("look up bridge %s: %w", bridgeName, err)
	}
	if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
		_ = netlink.LinkDel(hostLink)
		return fmt.Errorf("attach %s to %s: %w", hostIf, bridgeName, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		_ = netlink.LinkDel(hostLink)
		return fmt.Errorf("bring %s up: %w", hostIf, err)
	}
	contLink, err := netlink.LinkByName(contIf)
	if err != nil {
		_ = netlink.LinkDel(hostLink)
		return fmt.Errorf("look up %s: %w", contIf, err)
	}
	if err := netlink.LinkSetNsPid(contLink, pid); err != nil {
		_ = netlink.LinkDel(hostLink)
		return fmt.Errorf("move %s into the container network namespace (pid %d): %w", contIf, pid, err)
	}
	return nil
}

// configureContainerNetns sets the address, MTU and default route inside the
// container's network namespace, through a netlink handle bound to it.
func configureContainerNetns(pid int, contIf, containerIP, gateway string, ipnet *net.IPNet) error {
	target, err := netns.GetFromPath("/proc/" + strconv.Itoa(pid) + "/ns/net")
	if err != nil {
		return fmt.Errorf("open container network namespace: %w", err)
	}
	defer target.Close()
	handle, err := netlink.NewHandleAt(target)
	if err != nil {
		return fmt.Errorf("open netlink handle in the container namespace: %w", err)
	}
	defer handle.Close()

	link, err := handle.LinkByName(contIf)
	if err != nil {
		return fmt.Errorf("look up %s in the container namespace: %w", contIf, err)
	}
	if err := handle.LinkSetName(link, "eth0"); err != nil {
		return fmt.Errorf("rename %s to eth0: %w", contIf, err)
	}
	link, err = handle.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("look up eth0 after renaming: %w", err)
	}
	if err := handle.LinkSetMTU(link, 1500); err != nil {
		return fmt.Errorf("set eth0 MTU: %w", err)
	}
	addr, err := netlink.ParseAddr(containerIP + "/" + prefixLen(ipnet))
	if err != nil {
		return fmt.Errorf("parse container address %s: %w", containerIP, err)
	}
	if err := handle.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("assign %s to eth0: %w", containerIP, err)
	}
	if err := handle.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring eth0 up: %w", err)
	}
	gw := net.ParseIP(gateway)
	if gw == nil {
		return fmt.Errorf("invalid gateway %q", gateway)
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gw,
		Dst:       nil, // default route
	}
	if err := handle.RouteAdd(route); err != nil && !strings.Contains(err.Error(), "file exists") {
		return fmt.Errorf("add default route via %s: %w", gateway, err)
	}
	lo, err := handle.LinkByName("lo")
	if err == nil {
		_ = handle.LinkSetUp(lo)
	}
	return nil
}

// addHostRoute makes the container reachable from the host through its veth.
func addHostRoute(containerIP, hostIf string) error {
	link, err := netlink.LinkByName(hostIf)
	if err != nil {
		return fmt.Errorf("look up %s: %w", hostIf, err)
	}
	ip := net.ParseIP(containerIP)
	if ip == nil {
		return fmt.Errorf("invalid container address %q", containerIP)
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("add host route to %s: %w", containerIP, err)
	}
	return nil
}

// deleteLink removes a link, ignoring "not found".
func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete link %s: %w", name, err)
	}
	return nil
}

// hostDNSOrDefault returns an explicit DNS list or the host's resolvers.
func hostDNSOrDefault(dns []string) []string {
	if len(dns) > 0 {
		return dns
	}
	return HostDNS()
}

// writeNetworkState records the addresses assigned to a container.
func writeNetworkState(cfg Config, containerIP, gateway string) error {
	state := networkState{
		ContainerID: cfg.ContainerID,
		IP:          containerIP,
		Gateway:     gateway,
		Bridge:      BridgeName(cfg),
		Subnet:      Subnet(cfg),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(cfg, "network.json"), data, 0o600)
}

// networkState is the on-disk record of a container's networking.
type networkState struct {
	ContainerID string `json:"container_id"`
	IP          string `json:"ip"`
	Gateway     string `json:"gateway"`
	Bridge      string `json:"bridge"`
	Subnet      string `json:"subnet"`
}

// ReadContainerIP reads the container address recorded by Setup.
func ReadContainerIP(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "network.json"))
	if err != nil {
		return "", fmt.Errorf("read network state: %w", err)
	}
	var st networkState
	if err := json.Unmarshal(data, &st); err != nil {
		return "", fmt.Errorf("parse network state: %w", err)
	}
	return st.IP, nil
}

// nftAvailable reports whether the nft binary is on PATH.
func nftAvailable() bool {
	_, err := exec.LookPath("nft")
	return err == nil
}

// isNftMissing reports whether an error came from a missing nft binary.
func isNftMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "nft")
}
