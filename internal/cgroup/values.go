// Package cgroup implements cgroup v2 management for containers: creation,
// resource limits, OOM reporting and cleanup.
package cgroup

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseBytes parses a human-readable size such as "50m", "1G", "512k", "1024",
// "2GiB" or "100MB". Bare numbers are bytes; suffixes are powers of 1024, as
// Docker treats them for memory limits.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	for _, suffix := range []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"tb", 1 << 40},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suffix.suffix) {
			mult = suffix.mult
			lower = strings.TrimSuffix(lower, suffix.suffix)
			break
		}
	}
	lower = strings.TrimSpace(lower)
	if lower == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if strings.ContainsAny(lower, ".eE") {
		f, err := strconv.ParseFloat(lower, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		if f < 0 {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		return int64(f * float64(mult)), nil
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// CPUPeriodMicros is the cgroup v2 scheduling period minibox uses.
const CPUPeriodMicros = 100000

// ParseCPUs converts a CPU count such as "0.5" or "2" into a cpu.max quota for
// CPUPeriodMicros.
func ParseCPUs(s string) (quotaMicros int64, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cpu count")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu count %q", s)
	}
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid cpu count %q", s)
	}
	quota := int64(math.Round(f * CPUPeriodMicros))
	if quota < 1000 {
		quota = 1000
	}
	return quota, nil
}

// FormatCPUMax renders a cpu.max value.
func FormatCPUMax(quotaMicros int64) string {
	if quotaMicros <= 0 {
		return "max " + strconv.Itoa(CPUPeriodMicros)
	}
	return strconv.FormatInt(quotaMicros, 10) + " " + strconv.Itoa(CPUPeriodMicros)
}

// ParsePids parses a --pids-limit value. "0" or empty means unlimited.
func ParsePids(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid pids limit %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid pids limit %q", s)
	}
	return n, nil
}

// ParseSelfCgroupPath extracts the cgroup v2 path from the contents of
// /proc/self/cgroup, e.g. "0::/user.slice/foo" -> "/user.slice/foo".
func ParseSelfCgroupPath(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			p := parts[2]
			if p == "" {
				p = "/"
			}
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry found in /proc/self/cgroup")
}

// ParseKeyValues parses a cgroup v2 key/value file (one "key value" per line).
func ParseKeyValues(data string) map[string]int64 {
	out := make(map[string]int64)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = n
	}
	return out
}
