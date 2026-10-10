package security

import (
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// Seccomp BPF constants. The kernel's SECCOMP_RET_* values are not all exported
// by x/sys/unix, so they are defined here.
const (
	seccompRetKillProcess = 0x80000000
	seccompRetTrap        = 0x00030000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000
)

// Audit architecture tokens (the values found in seccomp_data.arch).
const (
	auditArchX86_64  = 0xc000003e
	auditArchAArch64 = 0xc00000b7
)

// BPF instruction classes and opcodes used to build the filter.
const (
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfJGE = 0x30
	bpfK   = 0x00
	bpfRET = 0x06
)

// Offsets inside struct seccomp_data.
const (
	seccompDataNr   = 0
	seccompDataArch = 4
)

// maxFilterInstructions is the kernel limit for a seccomp BPF program.
const maxFilterInstructions = 4096

// Action is a seccomp return action.
type Action uint32

// Errno returns an action that makes the syscall fail with err.
func Errno(err unix.Errno) Action { return Action(seccompRetErrno | uint32(err)) }

// KillProcess kills the whole container process when the syscall is used.
const KillProcess = Action(seccompRetKillProcess)

// TrapProcess delivers SIGSYS instead of running the syscall.
const TrapProcess = Action(seccompRetTrap)

// AllowAction runs the syscall.
const AllowAction = Action(seccompRetAllow)

// Rule is a single syscall rule.
type Rule struct {
	Syscall string
	Action  Action
}

// Profile describes a seccomp policy.
type Profile struct {
	Name string
	// Default is applied to syscalls not matched by any rule.
	Default Action
	// AllowEveryKnownSyscall seeds the allow list with every syscall known to
	// this architecture. This mirrors Docker's default profile, which permits
	// the full syscall table minus a documented dangerous set.
	AllowEveryKnownSyscall bool
	// Allow lists syscalls permitted explicitly (used when
	// AllowEveryKnownSyscall is false).
	Allow []string
	// Deny lists syscalls refused explicitly. Deny rules are emitted before
	// allow rules, so they win when a syscall appears in both lists.
	Deny []Rule
}

// DefaultDeniedSyscalls is the set of syscalls the default profile refuses,
// following Docker's default seccomp profile.
var DefaultDeniedSyscalls = []string{
	"_sysctl",
	"acct",
	"add_key",
	"adjtimex",
	"bpf",
	"clock_adjtime",
	"clock_settime",
	"create_module",
	"delete_module",
	"fanotify_init",
	"get_kernel_syms",
	"get_mempolicy",
	"init_module",
	"ioperm",
	"iopl",
	"kcmp",
	"kexec_file_load",
	"kexec_load",
	"keyctl",
	"lookup_dcookie",
	"mbind",
	"mount",
	"move_pages",
	"name_to_handle_at",
	"nfsservctl",
	"open_by_handle_at",
	"perf_event_open",
	"pivot_root",
	"process_vm_readv",
	"process_vm_writev",
	"ptrace",
	"query_module",
	"quotactl",
	"reboot",
	"request_key",
	"set_mempolicy",
	"setns",
	"settimeofday",
	"stime",
	"swapoff",
	"swapon",
	"sysfs",
	"umount",
	"umount2",
	"unshare",
	"uselib",
	"userfaultfd",
	"ustat",
	"vm86",
	"vm86old",
}

// DefaultProfile returns the profile used when the user does not ask for
// anything else: an allow-list with EPERM as the default action.
func DefaultProfile() Profile {
	deny := make([]Rule, 0, len(DefaultDeniedSyscalls))
	for _, name := range DefaultDeniedSyscalls {
		deny = append(deny, Rule{Syscall: name, Action: Errno(unix.EPERM)})
	}
	return Profile{
		Name:                   "default",
		Default:                Errno(unix.EPERM),
		AllowEveryKnownSyscall: true,
		Deny:                   deny,
	}
}

// nativeArch returns the AUDIT_ARCH_* token for the running architecture.
func nativeArch() (uint32, bool) {
	switch nativeArchName {
	case "amd64":
		return auditArchX86_64, true
	case "arm64":
		return auditArchAArch64, true
	default:
		return 0, false
	}
}

// BuildFilter compiles the profile into a seccomp BPF program.
func BuildFilter(p Profile) ([]unix.SockFilter, error) {
	arch, ok := nativeArch()
	if !ok {
		return nil, fmt.Errorf("seccomp is not supported on %s", nativeArchName)
	}

	allow := map[uint32]bool{}
	if p.AllowEveryKnownSyscall {
		for _, n := range syscallNumbers {
			allow[n] = true
		}
	}
	for _, name := range p.Allow {
		n, ok := syscallNumbers[name]
		if !ok {
			// Unknown names are ignored: profiles may be shared across
			// architectures where a syscall does not exist.
			continue
		}
		allow[n] = true
	}

	type rule struct {
		nr     uint32
		action Action
	}
	var rules []rule
	for _, d := range p.Deny {
		n, ok := syscallNumbers[d.Syscall]
		if !ok {
			continue
		}
		delete(allow, n)
		rules = append(rules, rule{nr: n, action: d.Action})
	}
	for n := range allow {
		rules = append(rules, rule{nr: n, action: AllowAction})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].nr < rules[j].nr })

	filter := make([]unix.SockFilter, 0, 5+2*len(rules))
	filter = append(filter,
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: seccompDataArch},
		// JEQ jumps to Jt when the architectures match, so Jt must skip over
		// the kill and Jf must fall through to it. Getting this backwards
		// kills every process on its first syscall.
		unix.SockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 1, Jf: 0, K: arch},
		unix.SockFilter{Code: bpfRET | bpfK, K: seccompRetKillProcess},
		unix.SockFilter{Code: bpfLD | bpfW | bpfABS, K: seccompDataNr},
	)
	if nativeArchName == "amd64" {
		// Reject the x32 ABI: its syscall numbers share the x86_64 table with
		// bit 30 set, which would let a process bypass the rules below.
		filter = append(filter,
			unix.SockFilter{Code: bpfJMP | bpfJGE | bpfK, Jt: 0, Jf: 1, K: 0x40000000},
			unix.SockFilter{Code: bpfRET | bpfK, K: seccompRetKillProcess},
		)
	}
	for _, r := range rules {
		filter = append(filter, unix.SockFilter{
			Code: bpfJMP | bpfJEQ | bpfK,
			Jt:   0,
			// Not equal: skip this rule's return and fall on the next rule's
			// test (or, after the last rule, on the default action below).
			Jf: 1,
			K:  r.nr,
		})
		filter = append(filter, unix.SockFilter{Code: bpfRET | bpfK, K: uint32(r.action)})
	}
	filter = append(filter, unix.SockFilter{Code: bpfRET | bpfK, K: uint32(p.Default)})

	if len(filter) > maxFilterInstructions {
		return nil, fmt.Errorf("seccomp filter too large: %d instructions (max %d)", len(filter), maxFilterInstructions)
	}
	return filter, nil
}

// SyscallNumber returns the number of a syscall by name on this architecture.
func SyscallNumber(name string) (uint32, bool) {
	n, ok := syscallNumbers[name]
	return n, ok
}

// KnownSyscallCount reports how many syscalls the filter builder knows about on
// this architecture.
func KnownSyscallCount() int { return len(syscallNumbers) }
