//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Status is a container's lifecycle state.
type Status string

// Container states.
const (
	StatusCreating Status = "creating"
	StatusCreated  Status = "created"
	StatusRunning  Status = "running"
	StatusExited   Status = "exited"
	StatusDead     Status = "dead"
)

// State is the mutable state of a container, stored in state.json.
type State struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	Image      string    `json:"image"`
	Command    string    `json:"command"`
	Status     Status    `json:"status"`
	PID        int       `json:"pid,omitempty"`
	ExitCode   int       `json:"exit_code"`
	OOMKilled  bool      `json:"oom_killed,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// LoadState reads a state file. A missing file yields a zero State.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &st, nil
}

// Save writes the state atomically (temp file plus rename).
func (st *State) Save(path string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// Refresh updates the live fields from the process table: a container marked
// running whose process is gone is reported as exited.
func (st *State) Refresh() {
	if st.Status != StatusRunning {
		return
	}
	if st.PID > 0 && !processAlive(st.PID) {
		st.Status = StatusExited
		st.FinishedAt = time.Now()
	}
}

// Container bundles a container's config and state.
type Container struct {
	ID     string
	Dir    string
	State  *State
	config []byte
}

// ConfigPath returns the path to the container's config.json.
func (c *Container) ConfigPath() string { return filepath.Join(c.Dir, "config.json") }

// StatePath returns the path to the container's state.json.
func (c *Container) StatePath() string { return filepath.Join(c.Dir, "state.json") }

// LogPath returns the path to the container's log file.
func (c *Container) LogPath() string { return filepath.Join(c.Dir, "log") }

// SaveState writes the container's state file.
func (c *Container) SaveState() error { return c.State.Save(c.StatePath()) }
