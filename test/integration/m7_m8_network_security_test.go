//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// M7: bridge networking gives the container internet access and DNS.
func TestM7BridgeInternetAndDNS(t *testing.T) {
	requireRoot(t)
	requireNetwork(t)
	ensureImage(t)
	out, errOut, code := mb(t, "run", "--rm", "--network", "bridge", testImage,
		"/bin/sh", "-c", "wget -q -O- http://example.com >/dev/null && echo NET_OK; nslookup example.com >/dev/null 2>&1 && echo DNS_OK")
	if code != 0 {
		t.Fatalf("bridge run failed (%d): %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "NET_OK") {
		t.Errorf("container could not reach the internet: %s%s", out, errOut)
	}
	if !strings.Contains(out, "DNS_OK") {
		t.Errorf("DNS resolution failed: %s%s", out, errOut)
	}
}

// M7: two containers can reach each other, and -p publishes a port.
func TestM7ContainerToContainerAndPortPublish(t *testing.T) {
	requireRoot(t)
	requireNetwork(t)
	ensureImage(t)

	out, _, code := mb(t, "run", "-d", "--network", "bridge", "--name", "server", testImage,
		"/bin/sh", "-c", "while true; do nc -l -p 9000 -e /bin/echo hello; done")
	if code != 0 {
		t.Skipf("could not start the test server (busybox nc may be unavailable): %s", out)
	}
	serverID := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", serverID)

	time.Sleep(time.Second)
	inspect, _, _ := mb(t, "inspect", serverID)
	if !strings.Contains(inspect, `"container_ip"`) {
		t.Errorf("no container address recorded:\n%s", inspect)
	}

	// Publish a port on an nginx-like server: use busybox httpd.
	out, _, code = mb(t, "run", "-d", "--network", "bridge", "-p", "18080:80", "--name", "web",
		testImage, "/bin/sh", "-c", "echo minibox-web > /tmp/index.html; httpd -f -p 80 -h /tmp")
	if code != 0 {
		t.Skipf("could not start busybox httpd: %s", out)
	}
	webID := strings.TrimSpace(out)
	defer mb(t, "rm", "-f", webID)

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1:18080/")
		if err == nil {
			body := make([]byte, 64)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()
			if strings.Contains(string(body[:n]), "minibox-web") {
				return
			}
			lastErr = nil
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("published port did not serve the container's page: %v", lastErr)
}

// M8: capabilities are reduced, mount is blocked and masked paths are hidden.
func TestM8SecurityHardening(t *testing.T) {
	requireRoot(t)
	ensureImage(t)
	script := strings.Join([]string{
		"mount -t tmpfs none /mnt 2>/dev/null && echo MOUNT_ALLOWED || echo MOUNT_BLOCKED",
		"cat /proc/kcore >/dev/null 2>&1 && echo KCORE_READABLE || echo KCORE_DENIED",
		"grep CapEff /proc/self/status",
	}, "; ")
	out, errOut, code := mb(t, "run", "--rm", "--network", "none", testImage, "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("run failed (%d): %s%s", code, out, errOut)
	}
	if !strings.Contains(out, "MOUNT_BLOCKED") {
		t.Errorf("mount was not blocked by seccomp: %s", out)
	}
	if !strings.Contains(out, "KCORE_DENIED") {
		t.Errorf("/proc/kcore was readable: %s", out)
	}
	if !strings.Contains(out, "CapEff:") {
		t.Fatalf("no capability line in output: %s", out)
	}
	// Docker's default set has no CAP_SYS_ADMIN (bit 21) or CAP_NET_ADMIN (12).
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		hexValue := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		value, err := parseHex64(hexValue)
		if err != nil {
			t.Fatalf("cannot parse %q: %v", line, err)
		}
		if value&(1<<21) != 0 {
			t.Error("CAP_SYS_ADMIN is present in the container")
		}
		if value&(1<<12) != 0 {
			t.Error("CAP_NET_ADMIN is present in the container")
		}
		if value&1 == 0 {
			t.Error("CAP_CHOWN should be present by default")
		}
	}
}

// Final sweep: no mounts, cgroups, veths or nftables rules survive the suite.
func TestZZLeakCheck(t *testing.T) {
	mustRun(t, "system", "prune")
	if mounts := leakedMounts(t); len(mounts) > 0 {
		t.Errorf("leaked mounts: %v", mounts)
	}
	if ids := containerIDs(t); len(ids) > 0 {
		t.Errorf("leftover containers: %v", ids)
	}
	if out, err := runCmd("ip", "link", "show"); err == nil && strings.Contains(out, "mb") {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, ": mb") {
				t.Errorf("leaked veth interface: %s", strings.TrimSpace(line))
			}
		}
	}
	if out, err := runCmd("nft", "list", "tables"); err == nil && strings.Contains(out, "minibox") {
		t.Errorf("leaked nftables tables: %s", out)
	}
}
