//go:build linux

package network

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

// lease is one allocated container address.
type lease struct {
	ContainerID string    `json:"container_id"`
	IP          string    `json:"ip"`
	Allocated   time.Time `json:"allocated"`
}

// ipamDir is where leases live, under the store root.
func ipamDir(stateDir string) string {
	// stateDir is <root>/containers/<id>; leases live in <root>/networks/ipam.
	return filepath.Join(filepath.Dir(filepath.Dir(stateDir)), "networks", "ipam")
}

// AllocateIP hands out the next free address from the bridge subnet.
func AllocateIP(stateDir, containerID, subnet string) (string, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("invalid subnet %q: %w", subnet, err)
	}
	dir := ipamDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	lock, err := container.Acquire(filepath.Join(dir, "ipam.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Release()

	leases, err := readLeases(dir)
	if err != nil {
		return "", err
	}
	// Reuse this container's existing lease when it has one.
	for _, l := range leases {
		if l.ContainerID == containerID {
			return l.IP, nil
		}
	}
	used := map[string]bool{}
	for _, l := range leases {
		used[l.IP] = true
	}
	// The gateway is the network's first usable address; containers start at
	// the second.
	base := ipnet.IP.To4()
	if base == nil {
		return "", fmt.Errorf("subnet %q is not IPv4", subnet)
	}
	for host := 2; host < 1<<16; host++ {
		ip := offsetIP(ipnet, host)
		if ip == nil || !ipnet.Contains(ip) {
			break
		}
		s := ip.String()
		if used[s] {
			continue
		}
		l := lease{ContainerID: containerID, IP: s, Allocated: time.Now().UTC()}
		if err := writeLease(dir, containerID, l); err != nil {
			return "", err
		}
		return s, nil
	}
	return "", fmt.Errorf("the container subnet %s is exhausted: remove unused containers or choose another subnet", subnet)
}

// offsetIP returns the address at the given host offset within a network.
func offsetIP(network *net.IPNet, offset int) net.IP {
	base := network.IP.To4()
	if base == nil {
		return nil
	}
	ip := make(net.IP, 4)
	copy(ip, base)
	value := int(ip[0])<<24 | int(ip[1])<<16 | int(ip[2])<<8 | int(ip[3])
	value += offset
	out := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value)).To4()
	return out
}

// ReleaseIP returns an address to the pool.
func ReleaseIP(stateDir, containerID string) error {
	dir := ipamDir(stateDir)
	lock, err := container.Acquire(filepath.Join(dir, "ipam.lock"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer lock.Release()
	path := filepath.Join(dir, containerID+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readLeases reads every lease file in dir.
func readLeases(dir string) ([]lease, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []lease
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var l lease
		if err := json.Unmarshal(data, &l); err != nil {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func writeLease(dir, containerID string, l lease) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, containerID+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, containerID+".json"))
}
