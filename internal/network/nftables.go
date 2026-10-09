//go:build linux

package network

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// nftTable is the name of the nftables table minibox owns.
const nftTable = "minibox"

// buildNATRules renders the shared masquerade rules for the bridge subnet.
func buildNATRules(bridgeName, subnet, containerIP string, ports []PortMapping) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", nftTable)
	fmt.Fprintf(&b, "\tchain postrouting {\n")
	fmt.Fprintf(&b, "\t\ttype nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s ip daddr != %s masquerade comment \"minibox-nat-%s\"\n", subnet, subnet, subnet)
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "}\n")
	return b.String()
}

// portTableName is the per-container nftables table, so that removing a
// container's rules is a single, exact deletion.
func portTableName(containerID string) string {
	short := containerID
	if len(short) > 12 {
		short = short[:12]
	}
	return nftTable + "-" + short
}

// buildPortRules renders the DNAT rules for one container's published ports in
// its own table.
func buildPortRules(containerID, containerIP string, ports []PortMapping) string {
	var b strings.Builder
	table := portTableName(containerID)
	fmt.Fprintf(&b, "table inet %s {\n", table)
	fmt.Fprintf(&b, "\tchain prerouting {\n")
	fmt.Fprintf(&b, "\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
	for _, p := range ports {
		if p.HostPort == 0 {
			continue
		}
		hostIP := p.HostIP
		match := fmt.Sprintf("%s dport %d", protoOf(p), p.HostPort)
		if hostIP != "" {
			match = fmt.Sprintf("ip daddr %s %s", hostIP, match)
		}
		fmt.Fprintf(&b, "\t\t%s dnat to %s:%d comment \"minibox-port\"\n", match, containerIP, p.ContainerPort)
	}
	fmt.Fprintf(&b, "\t}\n}\n")
	return b.String()
}

// ensureNAT installs the masquerade rule and this container's port rules.
func ensureNAT(bridgeName, subnet, containerID, containerIP string, ports []PortMapping) error {
	if !nftAvailable() {
		return fmt.Errorf("port publishing and NAT need the nft binary, which is not on PATH: " +
			"install nftables (for example `apt-get install -y nftables`) or use --network host")
	}
	if err := applyNft(buildNATRules(bridgeName, subnet, containerIP, ports)); err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}
	if err := applyNft(buildPortRules(containerID, containerIP, ports)); err != nil {
		return err
	}
	return nil
}

// removeNAT deletes this container's port table and leaves shared rules alone
// (DeleteTable removes the shared masquerade table during prune).
func removeNAT(containerID string, ports []PortMapping) error {
	if !nftAvailable() {
		return nil
	}
	if len(ports) == 0 {
		return nil
	}
	cmd := exec.Command("nft", "delete", "table", "inet", portTableName(containerID))
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return nil
		}
		return fmt.Errorf("remove nftables rules for %s: %v: %s", containerID[:12], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func protoOf(p PortMapping) string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

// applyNft feeds rules to nft.
func applyNft(rules string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apply nftables rules: %v: %s\nrules:\n%s", err, strings.TrimSpace(stderr.String()), rules)
	}
	return nil
}

// DeleteTable removes the whole minibox nftables table (used by prune when no
// container is left).
func DeleteTable() error {
	if !nftAvailable() {
		return nil
	}
	cmd := exec.Command("nft", "delete", "table", "inet", nftTable)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "No such file or directory") {
			return nil
		}
		return fmt.Errorf("delete the %s nftables table: %v: %s", nftTable, err, strings.TrimSpace(string(out)))
	}
	return nil
}
