//go:build linux

package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// Health states reported in state.json.
const (
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// healthMonitor probes a running container and records the result in its state
// file, so `ps` and `inspect` can report it.
type healthMonitor struct {
	store *Store
	id    string
	cfg   *runtime.HealthCheck

	mu       sync.Mutex
	state    string
	failures int
}

// startHealthMonitor launches the probe loop when the container has a health
// check. It returns a stop function that is safe to call more than once.
func startHealthMonitor(store *Store, id string, cfg *runtime.Config) func() {
	if cfg.HealthCheck == nil {
		return func() {}
	}
	cmd, err := cfg.HealthCheck.Command()
	if err != nil || len(cmd) == 0 {
		return func() {}
	}
	cfg.HealthCheck.Normalize()
	m := &healthMonitor{store: store, id: id, cfg: cfg.HealthCheck, state: HealthStarting}
	m.write()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.loop(ctx, cmd)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// loop runs probes until the context is cancelled.
func (m *healthMonitor) loop(ctx context.Context, cmd []string) {
	start := time.Now()
	timer := time.NewTimer(m.cfg.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// During the start period failures are not counted, but a success
		// still marks the container healthy.
		grace := m.cfg.StartPeriod > 0 && time.Since(start) < m.cfg.StartPeriod
		m.probe(ctx, cmd, grace)
		timer.Reset(m.cfg.Interval)
	}
}

// probe runs one probe and updates the state.
func (m *healthMonitor) probe(ctx context.Context, cmd []string, grace bool) {
	ok := m.runProbe(ctx, cmd)
	m.mu.Lock()
	switch {
	case ok:
		m.state = HealthHealthy
		m.failures = 0
	case grace:
		// Keep "starting" while inside the start period.
	default:
		m.failures++
		if m.failures >= m.cfg.Retries {
			m.state = HealthUnhealthy
		}
	}
	m.mu.Unlock()
	m.write()
}

// runProbe executes the probe inside the container's namespaces through the
// exec-child helper, which is the only safe way to enter them from a
// multithreaded process.
func (m *healthMonitor) runProbe(ctx context.Context, cmd []string) bool {
	c, err := Load(m.store, m.id)
	if err != nil || c.State.PID <= 0 {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	args := append([]string{"exec-child", "--pid", strconv.Itoa(c.State.PID), "--config", c.ConfigPath(), "--"}, cmd...)
	probe := exec.CommandContext(probeCtx, self, args...)
	probe.Env = os.Environ()
	return probe.Run() == nil
}

// write records the current health in the container's state file.
func (m *healthMonitor) write() {
	c, err := Load(m.store, m.id)
	if err != nil {
		return
	}
	m.mu.Lock()
	state, failures := m.state, m.failures
	m.mu.Unlock()
	c.State.Health = state
	c.State.HealthFailures = failures
	_ = c.SaveState()
}

// ParseRestartPolicy splits a --restart value into its policy and retry limit.
func ParseRestartPolicy(value string) (policy string, maxRetries int, err error) {
	if value == "" {
		return "no", 0, nil
	}
	policy = value
	if i := indexByte(value, ':'); i >= 0 {
		policy = value[:i]
		n, convErr := strconv.Atoi(value[i+1:])
		if convErr != nil || n < 0 {
			return "", 0, fmt.Errorf("invalid restart policy %q: the retry count must be a non-negative number", value)
		}
		maxRetries = n
	}
	switch policy {
	case "no", "on-failure", "always", "unless-stopped":
		return policy, maxRetries, nil
	default:
		return "", 0, fmt.Errorf("invalid restart policy %q: use no, on-failure[:max], always or unless-stopped", value)
	}
}

// ShouldRestart reports whether a container with this policy should be started
// again after exiting with the given code.
func ShouldRestart(policy string, maxRetries, restarts, exitCode int) bool {
	name, limit, err := ParseRestartPolicy(policy)
	if err != nil || name == "no" {
		return false
	}
	if limit == 0 {
		limit = maxRetries
	}
	switch name {
	case "always", "unless-stopped":
		return limit == 0 || restarts < limit
	case "on-failure":
		if exitCode == 0 {
			return false
		}
		return limit == 0 || restarts < limit
	default:
		return false
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
