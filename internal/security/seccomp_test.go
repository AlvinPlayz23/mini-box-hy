package security

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDefaultProfileFilter(t *testing.T) {
	filter, err := BuildFilter(DefaultProfile())
	if err != nil {
		t.Fatal(err)
	}
	if len(filter) == 0 {
		t.Fatal("empty filter")
	}
	if len(filter) > maxFilterInstructions {
		t.Fatalf("filter too large: %d", len(filter))
	}
	// The program must start by checking the architecture and end with the
	// default action.
	if filter[0].K != seccompDataArch {
		t.Errorf("first instruction loads %#x, want arch offset", filter[0].K)
	}
	last := filter[len(filter)-1]
	if last.K != uint32(Errno(unix.EPERM)) {
		t.Errorf("final instruction action = %#x, want EPERM", last.K)
	}
	if !containsActionFor(t, filter, "mount", uint32(Errno(unix.EPERM))) {
		t.Error("mount is not denied")
	}
	if !containsActionFor(t, filter, "read", seccompRetAllow) {
		t.Error("read is not allowed")
	}
	if !containsActionFor(t, filter, "execve", seccompRetAllow) {
		t.Error("execve is not allowed")
	}
}

func TestStrictProfile(t *testing.T) {
	p := Profile{
		Name:    "strict",
		Default: Errno(unix.EPERM),
		Allow:   []string{"read", "write", "exit", "exit_group", "futex"},
	}
	filter, err := BuildFilter(p)
	if err != nil {
		t.Fatal(err)
	}
	if containsActionFor(t, filter, "socket", seccompRetAllow) {
		t.Error("strict profile allowed socket")
	}
	if !containsActionFor(t, filter, "write", seccompRetAllow) {
		t.Error("strict profile did not allow write")
	}
}

func TestUnknownSyscallNamesIgnored(t *testing.T) {
	p := Profile{
		Name:    "mixed",
		Default: Errno(unix.EPERM),
		Allow:   []string{"read", "definitely_not_a_syscall"},
		Deny:    []Rule{{Syscall: "also_not_real", Action: KillProcess}},
	}
	if _, err := BuildFilter(p); err != nil {
		t.Fatalf("BuildFilter: %v", err)
	}
}

func TestFilterInstructionCountIsLinear(t *testing.T) {
	small, err := BuildFilter(Profile{Name: "small", Default: Errno(unix.EPERM), Allow: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	full, err := BuildFilter(DefaultProfile())
	if err != nil {
		t.Fatal(err)
	}
	if len(full) <= len(small) {
		t.Errorf("full filter (%d) should be larger than small (%d)", len(full), len(small))
	}
}

// containsActionFor reports whether the filter contains an equality test for
// the named syscall immediately followed by the given action.
func containsActionFor(t *testing.T, filter []unix.SockFilter, name string, action uint32) bool {
	t.Helper()
	nr, ok := SyscallNumber(name)
	if !ok {
		t.Skipf("syscall %q not known on this architecture", name)
	}
	for i := 0; i+1 < len(filter); i++ {
		if filter[i].Code == (bpfJMP|bpfJEQ|bpfK) && filter[i].K == nr {
			if filter[i+1].Code == (bpfRET|bpfK) && filter[i+1].K == action {
				return true
			}
		}
	}
	return false
}
