//go:build linux

package container

import (
	"strings"
	"testing"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

func TestParseRestartPolicy(t *testing.T) {
	tests := []struct {
		in         string
		policy     string
		maxRetries int
		wantErr    bool
	}{
		{in: "", policy: "no"},
		{in: "no", policy: "no"},
		{in: "always", policy: "always"},
		{in: "unless-stopped", policy: "unless-stopped"},
		{in: "on-failure", policy: "on-failure"},
		{in: "on-failure:3", policy: "on-failure", maxRetries: 3},
		{in: "always:2", policy: "always", maxRetries: 2},
		{in: "on-failure:x", wantErr: true},
		{in: "sometimes", wantErr: true},
		{in: "on-failure:-1", wantErr: true},
	}
	for _, tt := range tests {
		policy, retries, err := ParseRestartPolicy(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRestartPolicy(%q) = %q, %d; want error", tt.in, policy, retries)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRestartPolicy(%q): %v", tt.in, err)
			continue
		}
		if policy != tt.policy || retries != tt.maxRetries {
			t.Errorf("ParseRestartPolicy(%q) = %q, %d; want %q, %d", tt.in, policy, retries, tt.policy, tt.maxRetries)
		}
	}
}

func TestShouldRestart(t *testing.T) {
	tests := []struct {
		policy   string
		restarts int
		exitCode int
		want     bool
	}{
		{policy: "no", exitCode: 1, want: false},
		{policy: "always", exitCode: 0, want: true},
		{policy: "always", exitCode: 1, want: true},
		{policy: "always:1", restarts: 0, exitCode: 1, want: true},
		{policy: "always:1", restarts: 1, exitCode: 1, want: false},
		{policy: "on-failure", exitCode: 0, want: false},
		{policy: "on-failure", exitCode: 2, want: true},
		{policy: "on-failure:2", restarts: 1, exitCode: 2, want: true},
		{policy: "on-failure:2", restarts: 2, exitCode: 2, want: false},
		{policy: "unless-stopped", exitCode: 0, want: true},
		{policy: "bogus", exitCode: 1, want: false},
	}
	for _, tt := range tests {
		if got := ShouldRestart(tt.policy, 0, tt.restarts, tt.exitCode); got != tt.want {
			t.Errorf("ShouldRestart(%q, restarts=%d, exit=%d) = %v, want %v", tt.policy, tt.restarts, tt.exitCode, got, tt.want)
		}
	}
}

func TestHealthCheckCommand(t *testing.T) {
	tests := []struct {
		test []string
		want string
	}{
		{test: nil, want: ""},
		{test: []string{"NONE"}, want: ""},
		{test: []string{"CMD", "curl", "-f", "http://localhost"}, want: "curl|-f|http://localhost"},
		{test: []string{"CMD-SHELL", "curl -f http://localhost || exit 1"}, want: "/bin/sh|-c|curl -f http://localhost || exit 1"},
		{test: []string{"echo hi"}, want: "/bin/sh|-c|echo hi"},
		{test: []string{"CMD"}, want: ""},
	}
	for _, tt := range tests {
		h := &runtime.HealthCheck{Test: tt.test}
		got, err := h.Command()
		if err != nil {
			t.Errorf("Command(%v): %v", tt.test, err)
			continue
		}
		if joined := strings.Join(got, "|"); joined != tt.want {
			t.Errorf("Command(%v) = %q, want %q", tt.test, joined, tt.want)
		}
	}
}

func TestHealthCheckNormalize(t *testing.T) {
	h := &runtime.HealthCheck{Test: []string{"CMD", "true"}}
	h.Normalize()
	if h.Interval != runtime.DefaultHealthInterval || h.Timeout != runtime.DefaultHealthTimeout || h.Retries != runtime.DefaultHealthRetries {
		t.Errorf("normalize = %+v", h)
	}
	custom := &runtime.HealthCheck{Interval: time.Second, Timeout: 2 * time.Second, Retries: 7}
	custom.Normalize()
	if custom.Interval != time.Second || custom.Timeout != 2*time.Second || custom.Retries != 7 {
		t.Errorf("normalize changed explicit values: %+v", custom)
	}
	var nilCheck *runtime.HealthCheck
	nilCheck.Normalize()
}
