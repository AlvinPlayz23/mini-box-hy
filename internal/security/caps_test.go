package security

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseCapability(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"CAP_CHOWN", 0},
		{"chown", 0},
		{"ChOwN", 0},
		{"CAP_SYS_ADMIN", 21},
		{"sys_admin", 21},
		{"CAP_NET_BIND_SERVICE", 10},
	}
	for _, tt := range tests {
		got, err := ParseCapability(tt.in)
		if err != nil {
			t.Errorf("ParseCapability(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseCapability(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "CAP_NOPE", "frobnicate", "CAP_"} {
		if _, err := ParseCapability(bad); err == nil {
			t.Errorf("ParseCapability(%q) succeeded, want error", bad)
		}
	}
}

func TestCapabilityName(t *testing.T) {
	if got := CapabilityName(0); got != "CAP_CHOWN" {
		t.Errorf("CapabilityName(0) = %q", got)
	}
	if got := CapabilityName(9999); got != "CAP_9999" {
		t.Errorf("CapabilityName(9999) = %q", got)
	}
}

func TestResolveCapabilities(t *testing.T) {
	base, err := ResolveCapabilities(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != len(DefaultCapabilities) {
		t.Errorf("default set has %d caps, want %d", len(base), len(DefaultCapabilities))
	}

	added, err := ResolveCapabilities([]string{"SYS_ADMIN"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != len(base)+1 {
		t.Errorf("cap-add SYS_ADMIN: %d caps, want %d", len(added), len(base)+1)
	}

	dropped, err := ResolveCapabilities(nil, []string{"NET_RAW"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != len(base)-1 {
		t.Errorf("cap-drop NET_RAW: %d caps, want %d", len(dropped), len(base)-1)
	}

	none, err := ResolveCapabilities(nil, []string{"ALL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("cap-drop ALL: %d caps, want 0", len(none))
	}

	both, err := ResolveCapabilities([]string{"ALL"}, []string{"ALL"})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != len(capabilities) {
		t.Errorf("add ALL after drop ALL: %d caps, want %d", len(both), len(capabilities))
	}

	if _, err := ResolveCapabilities([]string{"NOPE"}, nil); err == nil {
		t.Error("cap-add NOPE succeeded, want error")
	}
}

func TestCapabilityNamesSorted(t *testing.T) {
	names := CapabilityNames([]int{21, 0, 10})
	joined := strings.Join(names, ",")
	if joined != "CAP_CHOWN,CAP_NET_BIND_SERVICE,CAP_SYS_ADMIN" {
		t.Errorf("CapabilityNames = %q", joined)
	}
}

func TestParseCapabilityMask(t *testing.T) {
	status := "Name:\ttest\nCapInh:\t0000000000000000\nCapPrm:\t00000000a80c05fb\nCapBnd:\t00000000a80405fb\n"
	bounding, err := ParseCapabilityMask(status, "CapBnd")
	if err != nil {
		t.Fatal(err)
	}
	// 0xa80405fb has bit 13 (CAP_NET_RAW) clear and bit 0 (CAP_CHOWN) set.
	if bounding[13] {
		t.Error("CAP_NET_RAW should be absent from 0xa80405fb")
	}
	if !bounding[0] {
		t.Error("CAP_CHOWN should be present in 0xa80405fb")
	}
	if _, err := ParseCapabilityMask(status, "CapNope"); err == nil {
		t.Error("a missing field should be an error")
	}
	if _, err := ParseCapabilityMask("CapBnd:\tzzzz\n", "CapBnd"); err == nil {
		t.Error("an unparsable mask should be an error")
	}
}

func TestSplitGrantable(t *testing.T) {
	full := map[int]bool{}
	for _, c := range capabilities {
		full[c.Num] = true
	}
	requested, err := ResolveCapabilities(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A host with the full bounding set and CAP_SETPCAP effective grants all.
	available, unavailable := splitGrantable(requested, full, full, true)
	if len(available) != len(requested) || len(unavailable) != 0 {
		t.Errorf("full host: available %d/%d, unavailable %v", len(available), len(requested), unavailable)
	}

	// A host whose bounding set lacks CAP_NET_RAW: everything but NET_RAW.
	withoutNetRaw := map[int]bool{}
	for k, v := range full {
		withoutNetRaw[k] = v
	}
	delete(withoutNetRaw, unix.CAP_NET_RAW)
	available, unavailable = splitGrantable(requested, withoutNetRaw, full, true)
	if len(available) != len(requested)-1 {
		t.Errorf("restricted host: available %d, want %d", len(available), len(requested)-1)
	}
	if len(unavailable) != 1 || unavailable[0] != "CAP_NET_RAW" {
		t.Errorf("restricted host: unavailable %v, want [CAP_NET_RAW]", unavailable)
	}

	// A process that already lost its capabilities (setuid without KEEPCAPS):
	// nothing can be installed, and the caller warns instead of failing.
	available, unavailable = splitGrantable(requested, full, map[int]bool{}, false)
	if len(available) != 0 {
		t.Errorf("unprivileged process: available %v, want none", available)
	}
	if len(unavailable) != len(requested) {
		t.Errorf("unprivileged process: unavailable %d, want %d", len(unavailable), len(requested))
	}

	// Without CAP_SETPCAP the permitted set is the limit.
	permitted := map[int]bool{unix.CAP_CHOWN: true}
	available, unavailable = splitGrantable([]int{unix.CAP_CHOWN, unix.CAP_KILL}, full, permitted, false)
	if len(available) != 1 || available[0] != unix.CAP_CHOWN {
		t.Errorf("permitted-limited: available %v, want [CAP_CHOWN]", available)
	}
	if len(unavailable) != 1 || unavailable[0] != "CAP_KILL" {
		t.Errorf("permitted-limited: unavailable %v, want [CAP_KILL]", unavailable)
	}
}
