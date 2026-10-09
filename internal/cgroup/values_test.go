package cgroup

import "testing"

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1024", 1024},
		{"50m", 50 << 20},
		{"50M", 50 << 20},
		{"1g", 1 << 30},
		{"512k", 512 << 10},
		{"2GiB", 2 << 30},
		{"100MB", 100 << 20},
		{"1.5g", 1610612736},
		{"1b", 1},
	}
	for _, tt := range tests {
		got, err := ParseBytes(tt.in)
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "m", "abc", "10x", "-5m"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) succeeded, want error", bad)
		}
	}
}

func TestParseCPUs(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1", 100000},
		{"0.5", 50000},
		{"2", 200000},
		{"1.5", 150000},
		{"0.01", 1000},
	}
	for _, tt := range tests {
		got, err := ParseCPUs(tt.in)
		if err != nil {
			t.Errorf("ParseCPUs(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseCPUs(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		if _, err := ParseCPUs(bad); err == nil {
			t.Errorf("ParseCPUs(%q) succeeded, want error", bad)
		}
	}
}

func TestFormatCPUMax(t *testing.T) {
	if got, want := FormatCPUMax(50000), "50000 100000"; got != want {
		t.Errorf("FormatCPUMax = %q, want %q", got, want)
	}
	if got, want := FormatCPUMax(0), "max 100000"; got != want {
		t.Errorf("FormatCPUMax(0) = %q, want %q", got, want)
	}
}

func TestParseSelfCgroupPath(t *testing.T) {
	got, err := ParseSelfCgroupPath("0::/user.slice/user-1000.slice/session-1.scope\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/user.slice/user-1000.slice/session-1.scope"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got, err = ParseSelfCgroupPath("12:pids:/\n0::/\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/" {
		t.Errorf("got %q, want /", got)
	}
	if _, err := ParseSelfCgroupPath("12:pids:/\n"); err == nil {
		t.Error("expected error for cgroup v1 only content")
	}
}

func TestParseKeyValues(t *testing.T) {
	data := "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n"
	got := ParseKeyValues(data)
	if got["max"] != 3 || got["oom"] != 1 || got["oom_kill"] != 1 {
		t.Errorf("got %v", got)
	}
}
