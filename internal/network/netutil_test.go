//go:build linux

package network

import "net"

// netParseCIDR is a test helper.
func netParseCIDR(s string) (net.IP, *net.IPNet, error) { return net.ParseCIDR(s) }
