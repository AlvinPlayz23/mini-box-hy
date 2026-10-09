//go:build linux

package network

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"none", "host", "bridge", "pasta", ""} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q): %v", ok, err)
		}
	}
	if _, err := ParseMode("overlay"); err == nil {
		t.Error("ParseMode(\"overlay\") should fail")
	}
}

func TestParsePortMapping(t *testing.T) {
	tests := []struct {
		in        string
		hostIP    string
		hostPort  int
		contPort  int
		protocol  string
		wantError bool
	}{
		{in: "8080:80", hostPort: 8080, contPort: 80, protocol: "tcp"},
		{in: "127.0.0.1:8080:80", hostIP: "127.0.0.1", hostPort: 8080, contPort: 80, protocol: "tcp"},
		{in: "8080:80/udp", hostPort: 8080, contPort: 80, protocol: "udp"},
		{in: "53:53/UDP", hostPort: 53, contPort: 53, protocol: "udp"},
		{in: "80", hostPort: 0, contPort: 80, protocol: "tcp"},
		{in: "", wantError: true},
		{in: "abc:80", wantError: true},
		{in: "8080:0", wantError: true},
		{in: "8080:80/sctp", wantError: true},
		{in: "1:2:3:4", wantError: true},
		{in: "70000:80", wantError: true},
	}
	for _, tt := range tests {
		got, err := ParsePortMapping(tt.in)
		if tt.wantError {
			if err == nil {
				t.Errorf("ParsePortMapping(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortMapping(%q): %v", tt.in, err)
			continue
		}
		if got.HostIP != tt.hostIP || got.HostPort != tt.hostPort || got.ContainerPort != tt.contPort || got.Protocol != tt.protocol {
			t.Errorf("ParsePortMapping(%q) = %+v", tt.in, got)
		}
	}
}

func TestBuildNATRules(t *testing.T) {
	ports := []PortMapping{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
	}
	rules := buildNATRules("minibox0", "10.88.0.0/16", "10.88.0.2", ports)
	for _, want := range []string{
		"table inet minibox",
		"masquerade",
		"ip saddr 10.88.0.0/16",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules do not contain %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "dnat") {
		t.Errorf("shared rules must not contain per-container DNAT entries:\n%s", rules)
	}

	containerID := strings.Repeat("a", 64)
	portRules := buildPortRules(containerID, "10.88.0.2", ports)
	for _, want := range []string{
		"table inet minibox-" + containerID[:12],
		"tcp dport 8080 dnat to 10.88.0.2:80",
		"udp dport 5353 dnat to 10.88.0.2:53",
	} {
		if !strings.Contains(portRules, want) {
			t.Errorf("port rules do not contain %q:\n%s", want, portRules)
		}
	}
}

func TestBuildNATRulesNoPorts(t *testing.T) {
	rules := buildNATRules("minibox0", "10.88.0.0/16", "10.88.0.2", nil)
	if strings.Contains(rules, "prerouting") {
		t.Errorf("rules should have no prerouting chain without ports:\n%s", rules)
	}
	if !strings.Contains(rules, "masquerade") {
		t.Errorf("rules should still masquerade:\n%s", rules)
	}
}

func TestWriteResolvConfAndHosts(t *testing.T) {
	root := t.TempDir()
	if err := WriteResolvConf(root, []string{"1.1.1.1", "8.8.8.8"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc/resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "nameserver 1.1.1.1") || !strings.Contains(string(data), "nameserver 8.8.8.8") {
		t.Errorf("resolv.conf = %q", data)
	}
	if err := WriteHosts(root, "box", "10.88.0.2"); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(filepath.Join(root, "etc/hosts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "10.88.0.2\tbox") {
		t.Errorf("hosts = %q", hosts)
	}

	// A symlinked resolv.conf must be replaced, not followed.
	link := filepath.Join(root, "etc/resolv.conf")
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteResolvConf(root, []string{"9.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(outsideData) != "original" {
		t.Error("resolv.conf write followed a symlink out of the container root")
	}
}

func TestHostDNS(t *testing.T) {
	dns := HostDNS()
	if len(dns) == 0 {
		t.Fatal("HostDNS returned nothing")
	}
}

func TestAllocateIPUniqueAndReusable(t *testing.T) {
	containersDir := filepath.Join(t.TempDir(), "containers")
	stateDir := filepath.Join(containersDir, "c1")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := AllocateIP(stateDir, "c1", DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if first != "10.88.0.2" {
		t.Errorf("first address = %s, want 10.88.0.2", first)
	}
	again, err := AllocateIP(stateDir, "c1", DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("re-allocation returned %s, want the existing %s", again, first)
	}

	c2Dir := filepath.Join(containersDir, "c2")
	if err := os.MkdirAll(c2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	second, err := AllocateIP(c2Dir, "c2", DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Errorf("two containers got the same address %s", first)
	}

	if err := ReleaseIP(c2Dir, "c2"); err != nil {
		t.Fatal(err)
	}
	c3Dir := filepath.Join(containersDir, "c3")
	if err := os.MkdirAll(c3Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	third, err := AllocateIP(c3Dir, "c3", DefaultSubnet)
	if err != nil {
		t.Fatal(err)
	}
	if third != second {
		t.Errorf("released address was not reused: got %s, want %s", third, second)
	}
}

func TestAllocateIPConcurrent(t *testing.T) {
	base := filepath.Join(t.TempDir(), "containers")
	const workers = 8
	results := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(base, "c"+string(rune('a'+i)))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				errs[i] = err
				return
			}
			ip, err := AllocateIP(dir, "c"+string(rune('a'+i)), DefaultSubnet)
			results[i], errs[i] = ip, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if seen[results[i]] {
			t.Fatalf("duplicate address %s handed out concurrently", results[i])
		}
		seen[results[i]] = true
	}
}

func TestOffsetIP(t *testing.T) {
	_, ipnet, err := netParseCIDR("10.88.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if got := offsetIP(ipnet, 1).String(); got != "10.88.0.1" {
		t.Errorf("offsetIP(1) = %s", got)
	}
	if got := offsetIP(ipnet, 300).String(); got != "10.88.1.44" {
		t.Errorf("offsetIP(300) = %s", got)
	}
}

func TestBridgeAndSubnetDefaults(t *testing.T) {
	cfg := Config{}
	if BridgeName(cfg) != DefaultBridge {
		t.Errorf("BridgeName = %q", BridgeName(cfg))
	}
	if Subnet(cfg) != DefaultSubnet {
		t.Errorf("Subnet = %q", Subnet(cfg))
	}
	cfg = Config{Bridge: "br-test", Subnet: "192.168.99.0/24"}
	if BridgeName(cfg) != "br-test" || Subnet(cfg) != "192.168.99.0/24" {
		t.Errorf("overrides ignored: %q %q", BridgeName(cfg), Subnet(cfg))
	}
}
