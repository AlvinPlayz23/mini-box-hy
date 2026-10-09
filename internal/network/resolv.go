//go:build linux

package network

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteResolvConf writes nameservers into the container root. It is
// best-effort: a read-only rootfs keeps its existing file.
func WriteResolvConf(rootfsMerged string, dns []string) error {
	if rootfsMerged == "" || len(dns) == 0 {
		return nil
	}
	var b strings.Builder
	for _, ns := range dns {
		b.WriteString("nameserver " + ns + "\n")
	}
	return writeRootfsFile(filepath.Join(rootfsMerged, "etc", "resolv.conf"), b.String())
}

// WriteHosts writes a minimal hosts file into the container root.
func WriteHosts(rootfsMerged, hostname, containerIP string) error {
	if rootfsMerged == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	if containerIP != "" && hostname != "" {
		b.WriteString(containerIP + "\t" + hostname + "\n")
	}
	return writeRootfsFile(filepath.Join(rootfsMerged, "etc", "hosts"), b.String())
}

// writeRootfsFile replaces a file inside the container root without following
// symlinks out of it.
func writeRootfsFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		if os.IsPermission(err) || strings.Contains(err.Error(), "read-only") {
			return nil
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
