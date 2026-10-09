//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Prefix is the directory under the host's cgroup root that holds all minibox
// container cgroups.
const Prefix = "minibox"

// Limits describes the resource limits to apply to a container cgroup. Zero
// values mean "unset".
type Limits struct {
	MemoryBytes     int64 // memory.max
	MemorySwapBytes int64 // memory.swap.max (0 = unset)
	CPUQuotaMicros  int64 // cpu.max quota (0 = unset)
	PidsMax         int64 // pids.max (0 = unset)
}

// Events reports cgroup v2 memory.events counters.
type Events struct {
	Max     int64 `json:"max"`
	OOM     int64 `json:"oom"`
	OOMKill int64 `json:"oom_kill"`
	High    int64 `json:"high"`
	Low     int64 `json:"low"`
}

// OOMed reports whether the cgroup hit its memory limit.
func (e Events) OOMed() bool { return e.OOM > 0 || e.OOMKill > 0 }

// Manager owns the minibox subtree of the host's cgroup v2 hierarchy.
type Manager struct {
	// Root is the cgroup2 mount point, normally /sys/fs/cgroup.
	Root string
	// Parent is the cgroup path containers are created under, e.g.
	// /sys/fs/cgroup/user.slice/.../minibox.
	Parent string
	// controllersEnabled records whether subtree_control could be set up.
	controllersEnabled bool
}

// Available reports whether the running kernel exposes a cgroup v2 unified
// hierarchy.
func Available() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

// ErrNoV2 is returned when the host does not use the cgroup v2 unified
// hierarchy.
var ErrNoV2 = errors.New("cgroup v2 (unified hierarchy) is required: " +
	"/sys/fs/cgroup/cgroup.controllers is missing, so this host uses cgroup v1 or a hybrid layout; " +
	"boot with systemd.unified_cgroup_hierarchy=1 or use a host with cgroup v2")

// Detect locates the cgroup2 root and the minibox parent directory. It creates
// the parent and enables the memory, cpu and pids controllers for children.
func Detect() (*Manager, error) {
	if !Available() {
		return nil, ErrNoV2
	}
	root := "/sys/fs/cgroup"
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	selfPath, err := ParseSelfCgroupPath(string(data))
	if err != nil {
		return nil, ErrNoV2
	}
	parent := filepath.Join(root, selfPath, Prefix)
	m := &Manager{Root: root, Parent: parent}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create cgroup parent %s: %w (root is required unless your user slice delegates the cgroup controllers)", parent, err)
	}
	m.controllersEnabled = m.enableControllers()
	return m, nil
}

// enableControllers turns on the controllers minibox needs for the parent's
// children. It is best-effort: without delegation (for example a rootless
// session without systemd delegation) limits will be unavailable but the
// runtime still works.
func (m *Manager) enableControllers() bool {
	want := []string{"memory", "cpu", "pids"}
	availData, err := os.ReadFile(filepath.Join(m.Parent, "cgroup.controllers"))
	if err != nil {
		return false
	}
	avail := strings.Fields(string(availData))
	var enable []string
	for _, c := range want {
		for _, a := range avail {
			if a == c {
				enable = append(enable, "+"+c)
			}
		}
	}
	if len(enable) == 0 {
		return false
	}
	err = os.WriteFile(filepath.Join(m.Parent, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0)
	return err == nil
}

// ControllersEnabled reports whether resource limits can be applied.
func (m *Manager) ControllersEnabled() bool { return m.controllersEnabled }

// Cgroup is a single container's cgroup.
type Cgroup struct {
	mgr  *Manager
	Path string
}

// NewManagerAt builds a Manager rooted at an arbitrary cgroup2 mount, for tests
// and for running inside a container.
func NewManagerAt(root, parent string) *Manager {
	return &Manager{Root: root, Parent: parent}
}

// Create makes the cgroup directory for a container.
func (m *Manager) Create(id string) (*Cgroup, error) {
	if id == "" || strings.ContainsRune(id, '/') {
		return nil, fmt.Errorf("invalid cgroup id %q", id)
	}
	path := filepath.Join(m.Parent, id)
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create cgroup %s: %w", path, err)
	}
	return &Cgroup{mgr: m, Path: path}, nil
}

// Open returns the cgroup for an existing container id.
func (m *Manager) Open(id string) (*Cgroup, error) {
	path := filepath.Join(m.Parent, id)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open cgroup %s: %w", path, err)
	}
	return &Cgroup{mgr: m, Path: path}, nil
}

// List returns the ids of existing container cgroups.
func (m *Manager) List() ([]string, error) {
	entries, err := os.ReadDir(m.Parent)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// FD opens a directory file descriptor suitable for CLONE_INTO_CGROUP.
func (c *Cgroup) FD() (int, error) {
	fd, err := unix.Open(c.Path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open cgroup fd %s: %w", c.Path, err)
	}
	return fd, nil
}

// AddPid moves a process into the cgroup.
func (c *Cgroup) AddPid(pid int) error {
	if err := c.write("cgroup.procs", strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("move pid %d into %s: %w", pid, c.Path, err)
	}
	return nil
}

// SetLimits applies resource limits. It returns a descriptive error when the
// controllers were not delegated to minibox.
func (c *Cgroup) SetLimits(l Limits) error {
	if l.MemoryBytes > 0 {
		if err := c.write("memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return c.limitError("memory", err)
		}
		if l.MemorySwapBytes == 0 {
			// Default to no extra swap, like Docker's default of swap==memory.
			if err := c.write("memory.swap.max", "0"); err != nil {
				return c.limitError("memory", err)
			}
		}
	}
	if l.MemorySwapBytes > 0 {
		swap := l.MemorySwapBytes - l.MemoryBytes
		if swap < 0 {
			swap = 0
		}
		if err := c.write("memory.swap.max", strconv.FormatInt(swap, 10)); err != nil {
			return c.limitError("memory", err)
		}
	}
	if l.CPUQuotaMicros > 0 {
		if err := c.write("cpu.max", FormatCPUMax(l.CPUQuotaMicros)); err != nil {
			return c.limitError("cpu", err)
		}
	}
	if l.PidsMax > 0 {
		if err := c.write("pids.max", strconv.FormatInt(l.PidsMax, 10)); err != nil {
			return c.limitError("pids", err)
		}
	}
	return nil
}

func (c *Cgroup) limitError(controller string, err error) error {
	return fmt.Errorf("set %s limit in %s: %w\n"+
		"hint: the %s controller must be delegated to minibox (cgroup.subtree_control in %s); "+
		"run as root or enable systemd delegation for this user",
		controller, c.Path, err, controller, c.mgr.Parent)
}

// Events reads memory.events.
func (c *Cgroup) Events() (Events, error) {
	data, err := os.ReadFile(filepath.Join(c.Path, "memory.events"))
	if err != nil {
		return Events{}, fmt.Errorf("read memory.events: %w", err)
	}
	kv := ParseKeyValues(string(data))
	return Events{
		Max:     kv["max"],
		OOM:     kv["oom"],
		OOMKill: kv["oom_kill"],
		High:    kv["high"],
		Low:     kv["low"],
	}, nil
}

// MemoryCurrent reports memory.current in bytes.
func (c *Cgroup) MemoryCurrent() (int64, error) {
	return c.readInt("memory.current")
}

// MemoryPeak reports memory.peak in bytes (kernel 5.19+); it returns -1 when
// the kernel does not provide the file.
func (c *Cgroup) MemoryPeak() int64 {
	n, err := c.readInt("memory.peak")
	if err != nil {
		return -1
	}
	return n
}

// PidsCurrent reports pids.current.
func (c *Cgroup) PidsCurrent() (int64, error) {
	return c.readInt("pids.current")
}

// Pids lists the PIDs currently in the cgroup.
func (c *Cgroup) Pids() ([]int, error) {
	data, err := os.ReadFile(filepath.Join(c.Path, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(line)
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// Empty reports whether the cgroup holds no processes.
func (c *Cgroup) Empty() bool {
	pids, err := c.Pids()
	return err == nil && len(pids) == 0
}

// Kill terminates every process in the cgroup, using cgroup.kill when the
// kernel provides it and SIGKILL to each pid otherwise.
func (c *Cgroup) Kill() error {
	if err := c.write("cgroup.kill", "1"); err == nil {
		return nil
	}
	pids, err := c.Pids()
	if err != nil {
		return err
	}
	var firstErr error
	for _, pid := range pids {
		if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Remove deletes the cgroup directory, retrying briefly because the kernel may
// need a moment to release the last reference.
func (c *Cgroup) Remove() error {
	var err error
	for i := 0; i < 20; i++ {
		err = unix.Rmdir(c.Path)
		if err == nil || err == unix.ENOENT {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("remove cgroup %s: %w", c.Path, err)
}

// Prune removes container cgroups that hold no processes, plus the parent when
// it is empty. It returns the ids it removed.
func (m *Manager) Prune() ([]string, error) {
	ids, err := m.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range ids {
		c, err := m.Open(id)
		if err != nil {
			continue
		}
		if !c.Empty() {
			continue
		}
		if err := c.Remove(); err == nil {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// RemoveIfEmpty removes the cgroup only when no process is left in it.
func (c *Cgroup) RemoveIfEmpty() error {
	if !c.Empty() {
		return fmt.Errorf("cgroup %s still has processes", c.Path)
	}
	return c.Remove()
}

func (c *Cgroup) write(file, value string) error {
	return os.WriteFile(filepath.Join(c.Path, file), []byte(value), 0)
}

func (c *Cgroup) readInt(file string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(c.Path, file))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
