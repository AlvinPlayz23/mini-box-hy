package security

import (
	"strings"
	"testing"
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
