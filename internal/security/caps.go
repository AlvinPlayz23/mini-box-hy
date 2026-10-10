// Package security implements Linux capability handling, seccomp BPF filter
// generation and the default security profiles used by minibox.
package security

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// capabilitySets reads the bounding, permitted and effective masks of the
// calling process from /proc/self/status.
func capabilitySets() (bounding, permitted, effective map[int]bool, err error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read /proc/self/status: %w", err)
	}
	status := string(data)
	if bounding, err = ParseCapabilityMask(status, "CapBnd"); err != nil {
		return nil, nil, nil, err
	}
	if permitted, err = ParseCapabilityMask(status, "CapPrm"); err != nil {
		return nil, nil, nil, err
	}
	if effective, err = ParseCapabilityMask(status, "CapEff"); err != nil {
		return nil, nil, nil, err
	}
	return bounding, permitted, effective, nil
}

// ParseCapabilityMask extracts one capability mask (for example "CapBnd") from
// the contents of /proc/self/status.
func ParseCapabilityMask(status, field string) (map[int]bool, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, field+":") {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, field+":")), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s value %q: %w", field, line, err)
		}
		out := map[int]bool{}
		for bit := 0; bit <= unix.CAP_LAST_CAP && bit < 64; bit++ {
			if mask&(1<<uint(bit)) != 0 {
				out[bit] = true
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("no %s line in /proc/self/status", field)
}

// SplitGrantable separates requested capabilities into the ones this process
// can actually install and the names of those it cannot.
//
// Two restrictions are common in practice and both must degrade to a warning
// rather than a failed container:
//
//   - the bounding set is smaller than the requested set (Railway, Kubernetes
//     restricted profiles, gVisor and nested containers all drop capabilities);
//   - the process already lost its capabilities, which is what setuid does when
//     the image or --user names a non-root user. Without CAP_SETPCAP in the
//     effective set the kernel only lets a process narrow its permitted set.
func SplitGrantable(caps []int) (available []int, unavailable []string) {
	bounding, permitted, effective, err := capabilitySets()
	if err != nil {
		// Without the masks, assume everything is available and let capset
		// decide; the error path then reports the real reason.
		return caps, nil
	}
	return splitGrantable(caps, bounding, permitted, effective[unix.CAP_SETPCAP])
}

// splitGrantable is the pure decision behind SplitGrantable.
func splitGrantable(caps []int, bounding, permitted map[int]bool, haveSetPCAP bool) (available []int, unavailable []string) {
	for _, c := range caps {
		if bounding[c] && (haveSetPCAP || permitted[c]) {
			available = append(available, c)
			continue
		}
		unavailable = append(unavailable, CapabilityName(c))
	}
	sort.Strings(unavailable)
	return available, unavailable
}

// capName is a Linux capability name and its number.
type capName struct {
	Name string
	Num  int
}

// capabilities is the full Linux capability table, in kernel order.
var capabilities = []capName{
	{"CAP_CHOWN", unix.CAP_CHOWN},
	{"CAP_DAC_OVERRIDE", unix.CAP_DAC_OVERRIDE},
	{"CAP_DAC_READ_SEARCH", unix.CAP_DAC_READ_SEARCH},
	{"CAP_FOWNER", unix.CAP_FOWNER},
	{"CAP_FSETID", unix.CAP_FSETID},
	{"CAP_KILL", unix.CAP_KILL},
	{"CAP_SETGID", unix.CAP_SETGID},
	{"CAP_SETUID", unix.CAP_SETUID},
	{"CAP_SETPCAP", unix.CAP_SETPCAP},
	{"CAP_LINUX_IMMUTABLE", unix.CAP_LINUX_IMMUTABLE},
	{"CAP_NET_BIND_SERVICE", unix.CAP_NET_BIND_SERVICE},
	{"CAP_NET_BROADCAST", unix.CAP_NET_BROADCAST},
	{"CAP_NET_ADMIN", unix.CAP_NET_ADMIN},
	{"CAP_NET_RAW", unix.CAP_NET_RAW},
	{"CAP_IPC_LOCK", unix.CAP_IPC_LOCK},
	{"CAP_IPC_OWNER", unix.CAP_IPC_OWNER},
	{"CAP_SYS_MODULE", unix.CAP_SYS_MODULE},
	{"CAP_SYS_RAWIO", unix.CAP_SYS_RAWIO},
	{"CAP_SYS_CHROOT", unix.CAP_SYS_CHROOT},
	{"CAP_SYS_PTRACE", unix.CAP_SYS_PTRACE},
	{"CAP_SYS_PACCT", unix.CAP_SYS_PACCT},
	{"CAP_SYS_ADMIN", unix.CAP_SYS_ADMIN},
	{"CAP_SYS_BOOT", unix.CAP_SYS_BOOT},
	{"CAP_SYS_NICE", unix.CAP_SYS_NICE},
	{"CAP_SYS_RESOURCE", unix.CAP_SYS_RESOURCE},
	{"CAP_SYS_TIME", unix.CAP_SYS_TIME},
	{"CAP_SYS_TTY_CONFIG", unix.CAP_SYS_TTY_CONFIG},
	{"CAP_MKNOD", unix.CAP_MKNOD},
	{"CAP_LEASE", unix.CAP_LEASE},
	{"CAP_AUDIT_WRITE", unix.CAP_AUDIT_WRITE},
	{"CAP_AUDIT_CONTROL", unix.CAP_AUDIT_CONTROL},
	{"CAP_SETFCAP", unix.CAP_SETFCAP},
	{"CAP_MAC_OVERRIDE", unix.CAP_MAC_OVERRIDE},
	{"CAP_MAC_ADMIN", unix.CAP_MAC_ADMIN},
	{"CAP_SYSLOG", unix.CAP_SYSLOG},
	{"CAP_WAKE_ALARM", unix.CAP_WAKE_ALARM},
	{"CAP_BLOCK_SUSPEND", unix.CAP_BLOCK_SUSPEND},
	{"CAP_AUDIT_READ", unix.CAP_AUDIT_READ},
	{"CAP_PERFMON", unix.CAP_PERFMON},
	{"CAP_BPF", unix.CAP_BPF},
	{"CAP_CHECKPOINT_RESTORE", unix.CAP_CHECKPOINT_RESTORE},
}

// DefaultCapabilities is the Docker-like default capability set for a
// container running as root.
var DefaultCapabilities = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_RAW",
	"CAP_SYS_CHROOT",
	"CAP_MKNOD",
	"CAP_AUDIT_WRITE",
	"CAP_SETFCAP",
}

// capNumbers maps capability names (with and without the CAP_ prefix) to
// numbers.
var capNumbers = func() map[string]int {
	m := make(map[string]int, len(capabilities)*2)
	for _, c := range capabilities {
		m[c.Name] = c.Num
		m[strings.TrimPrefix(c.Name, "CAP_")] = c.Num
	}
	return m
}()

// capNames maps capability numbers back to their canonical names.
var capNames = func() map[int]string {
	m := make(map[int]string, len(capabilities))
	for _, c := range capabilities {
		m[c.Num] = c.Name
	}
	return m
}()

// ParseCapability resolves a capability name such as "chown", "CHOWN" or
// "CAP_CHOWN" to its number.
func ParseCapability(s string) (int, error) {
	key := strings.ToUpper(strings.TrimSpace(s))
	if n, ok := capNumbers[key]; ok {
		return n, nil
	}
	if n, err := strconv.Atoi(key); err == nil && n >= 0 && n <= unix.CAP_LAST_CAP {
		return n, nil
	}
	return 0, fmt.Errorf("unknown capability %q", s)
}

// CapabilityName returns the canonical name for a capability number.
func CapabilityName(n int) string {
	if name, ok := capNames[n]; ok {
		return name
	}
	return fmt.Sprintf("CAP_%d", n)
}

// ParseCapabilities resolves a list of capability names to numbers.
func ParseCapabilities(names []string) ([]int, error) {
	nums := make([]int, 0, len(names))
	for _, name := range names {
		n, err := ParseCapability(name)
		if err != nil {
			return nil, err
		}
		nums = append(nums, n)
	}
	return nums, nil
}

// CapabilityNames converts numbers back to canonical names.
func CapabilityNames(nums []int) []string {
	names := make([]string, 0, len(nums))
	for _, n := range nums {
		names = append(names, CapabilityName(n))
	}
	sort.Strings(names)
	return names
}

// ResolveCapabilities computes the effective capability set from the defaults
// plus --cap-add / --cap-drop style arguments. Entries may be names or "ALL".
func ResolveCapabilities(add, drop []string) ([]int, error) {
	set := make(map[int]bool)
	for _, name := range DefaultCapabilities {
		n, err := ParseCapability(name)
		if err != nil {
			return nil, err
		}
		set[n] = true
	}
	apply := func(list []string, add bool) error {
		for _, name := range list {
			if strings.EqualFold(name, "ALL") {
				for _, c := range capabilities {
					if add {
						set[c.Num] = true
					} else {
						delete(set, c.Num)
					}
				}
				continue
			}
			n, err := ParseCapability(name)
			if err != nil {
				return err
			}
			if add {
				set[n] = true
			} else {
				delete(set, n)
			}
		}
		return nil
	}
	if err := apply(drop, false); err != nil {
		return nil, err
	}
	if err := apply(add, true); err != nil {
		return nil, err
	}
	nums := make([]int, 0, len(set))
	for n := range set {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	return nums, nil
}

// ApplyCapabilities installs the given capability set on the calling thread:
// permitted/effective/inheritable are set to caps, the bounding set is reduced
// to caps, and the ambient set is cleared. It must run after uid/gid changes.
//
// Capabilities the host's bounding set does not provide are skipped and
// returned by name, so a restricted host (Railway, Kubernetes restricted
// profiles, gVisor, nested containers) still runs containers instead of failing
// with "capset: operation not permitted". Callers should warn about the
// returned names.
func ApplyCapabilities(caps []int) (unavailable []string, err error) {
	grantable, unavailable := SplitGrantable(caps)
	want := make(map[int]bool, len(caps))
	for _, c := range grantable {
		want[c] = true
	}
	// Keep capabilities across the uid transition: needed when a non-root
	// --user is combined with --cap-add.
	if err := unix.Prctl(unix.PR_SET_KEEPCAPS, 1, 0, 0, 0); err != nil {
		return unavailable, fmt.Errorf("prctl(PR_SET_KEEPCAPS): %w", err)
	}

	var data [2]unix.CapUserData
	for _, c := range grantable {
		if c < 0 || c > unix.CAP_LAST_CAP {
			return unavailable, fmt.Errorf("capability %d out of range", c)
		}
		bit := uint32(1) << (uint(c) % 32)
		data[c/32].Effective |= bit
		data[c/32].Permitted |= bit
		data[c/32].Inheritable |= bit
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		if len(unavailable) > 0 {
			return unavailable, fmt.Errorf("capset %v: %w (this host's bounding set also lacks %s)",
				CapabilityNames(grantable), err, strings.Join(unavailable, ", "))
		}
		return unavailable, fmt.Errorf("capset: %w", err)
	}

	// Reduce the bounding set: caps dropped here can never be regained, even
	// through setuid binaries. Dropping needs CAP_SETPCAP, which the caller may
	// have asked us to drop, and which a user namespace does not need; both
	// cases are tolerated, and no_new_privs still blocks privilege gains.
	for _, c := range capabilities {
		if want[c.Num] {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c.Num), 0, 0, 0); err != nil &&
			err != unix.EINVAL && err != unix.EPERM {
			return unavailable, fmt.Errorf("prctl(PR_CAPBSET_DROP, %s): %w", c.Name, err)
		}
	}

	// Clear ambient capabilities so execve does not re-raise them.
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil &&
		err != unix.EINVAL && err != unix.EPERM {
		return unavailable, fmt.Errorf("prctl(PR_CAP_AMBIENT_CLEAR_ALL): %w", err)
	}
	return unavailable, nil
}

// SetNoNewPrivs sets PR_SET_NO_NEW_PRIVS, preventing privilege gains through
// execve (setuid binaries, file capabilities).
func SetNoNewPrivs() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	return nil
}
