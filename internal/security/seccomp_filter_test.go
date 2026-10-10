package security

import (
	"testing"

	"golang.org/x/sys/unix"
)

// evalFilter runs a generated BPF program the way the kernel does for one
// syscall, returning the action it decides. Tests use it because a filter that
// is structurally right can still be functionally inverted: only evaluating it
// against real seccomp_data catches a swapped jump.
func evalFilter(t *testing.T, filter []unix.SockFilter, arch, nr uint32) uint32 {
	t.Helper()
	data := func(offset uint32) uint32 {
		switch offset {
		case seccompDataNr:
			return nr
		case seccompDataArch:
			return arch
		default:
			return 0 // instruction pointer and argument words
		}
	}
	a := uint32(0)
	for pc, steps := 0, 0; steps < 4096; steps++ {
		if pc < 0 || pc >= len(filter) {
			t.Fatalf("filter left its own program at %d (%d instructions)", pc, len(filter))
		}
		ins := filter[pc]
		switch ins.Code {
		case bpfLD | bpfW | bpfABS:
			a = data(ins.K)
			pc++
		case bpfJMP | bpfJEQ | bpfK:
			if a == ins.K {
				pc += 1 + int(ins.Jt)
			} else {
				pc += 1 + int(ins.Jf)
			}
		case bpfJMP | bpfJGE | bpfK:
			if a >= ins.K {
				pc += 1 + int(ins.Jt)
			} else {
				pc += 1 + int(ins.Jf)
			}
		case bpfRET | bpfK:
			return ins.K
		default:
			t.Fatalf("unsupported instruction %#x at %d", ins.Code, pc)
		}
	}
	t.Fatal("filter did not terminate")
	return 0
}

// TestFilterDecisions is the regression test for the inverted architecture
// check: the filter must kill only on a foreign architecture or the x32 ABI,
// never on a native syscall.
func TestFilterDecisions(t *testing.T) {
	filter, err := BuildFilter(DefaultProfile())
	if err != nil {
		t.Fatal(err)
	}
	native, ok := nativeArch()
	if !ok {
		t.Skip("no native arch token on this platform")
	}
	syscallNr := func(name string) uint32 {
		nr, ok := SyscallNumber(name)
		if !ok {
			t.Skipf("syscall %q is not known on this architecture", name)
		}
		return nr
	}

	cases := []struct {
		name string
		arch uint32
		nr   uint32
		want uint32
	}{
		{"native read is allowed", native, syscallNr("read"), seccompRetAllow},
		{"native write is allowed", native, syscallNr("write"), seccompRetAllow},
		{"native execve is allowed", native, syscallNr("execve"), seccompRetAllow},
		{"native clone is allowed", native, syscallNr("clone"), seccompRetAllow},
		{"native mount is denied", native, syscallNr("mount"), uint32(Errno(unix.EPERM))},
		{"native ptrace is denied", native, syscallNr("ptrace"), uint32(Errno(unix.EPERM))},
		{"unknown syscall falls back to the default", native, 999, uint32(Errno(unix.EPERM))},
		{"foreign arch is killed", 0x40000003, syscallNr("read"), seccompRetKillProcess},
	}
	if nativeArchName == "amd64" {
		cases = append(cases, struct {
			name string
			arch uint32
			nr   uint32
			want uint32
		}{"x32 abi is killed", native, 0x40000000 | syscallNr("read"), seccompRetKillProcess})
	}
	for _, tc := range cases {
		got := evalFilter(t, filter, tc.arch, tc.nr)
		if got != tc.want {
			t.Errorf("%s: action %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

// TestFilterNeverKillsNativeSyscalls sweeps the syscall table: with the native
// architecture in seccomp_data, no number may ever reach the kill action. This
// is what fails when the architecture check is inverted.
func TestFilterNeverKillsNativeSyscalls(t *testing.T) {
	native, ok := nativeArch()
	if !ok {
		t.Skip("no native arch token on this platform")
	}
	cases := []struct {
		name    string
		profile Profile
		allowed []string
	}{
		{
			name:    "default",
			profile: DefaultProfile(),
			allowed: []string{"read", "write", "execve", "futex", "clone", "openat", "mmap", "rt_sigreturn"},
		},
		{
			name:    "strict",
			profile: Profile{Name: "strict", Default: Errno(unix.EPERM), Allow: []string{"read", "write"}},
			allowed: []string{"read", "write"},
		},
	}
	for _, tc := range cases {
		filter, err := BuildFilter(tc.profile)
		if err != nil {
			t.Fatal(err)
		}
		for nr := uint32(0); nr < 512; nr++ {
			if got := evalFilter(t, filter, native, nr); got == seccompRetKillProcess {
				t.Fatalf("profile %q kills syscall %d on the native architecture", tc.profile.Name, nr)
			}
		}
		for _, name := range tc.allowed {
			nr, ok := SyscallNumber(name)
			if !ok {
				continue
			}
			if got := evalFilter(t, filter, native, nr); got != seccompRetAllow {
				t.Errorf("profile %q returns %#x for %s, want allow", tc.profile.Name, got, name)
			}
		}
	}
}

// TestFilterKillsForeignArch ensures the architecture guard is still reachable:
// a filter that never kills would let a foreign-ABI process bypass every rule.
func TestFilterKillsForeignArch(t *testing.T) {
	filter, err := BuildFilter(DefaultProfile())
	if err != nil {
		t.Fatal(err)
	}
	if got := evalFilter(t, filter, 0xdeadbeef, 0); got != seccompRetKillProcess {
		t.Errorf("foreign architecture action %#x, want kill", got)
	}
}
