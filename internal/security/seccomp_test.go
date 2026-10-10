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
	// Decisions are asserted through the interpreter, because a filter can be
	// structurally plausible and still decide the wrong action (see
	// seccomp_filter_test.go).
	native, ok := nativeArch()
	if !ok {
		t.Skip("no native arch token on this platform")
	}
	for _, tc := range []struct {
		syscall string
		want    uint32
	}{
		{"read", seccompRetAllow},
		{"execve", seccompRetAllow},
		{"mount", uint32(Errno(unix.EPERM))},
	} {
		nr, ok := SyscallNumber(tc.syscall)
		if !ok {
			continue
		}
		if got := evalFilter(t, filter, native, nr); got != tc.want {
			t.Errorf("%s decides %#x, want %#x", tc.syscall, got, tc.want)
		}
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
	native, ok := nativeArch()
	if !ok {
		t.Skip("no native arch token on this platform")
	}
	for _, tc := range []struct {
		syscall string
		want    uint32
	}{
		{"write", seccompRetAllow},
		{"socket", uint32(Errno(unix.EPERM))},
		{"read", seccompRetAllow},
	} {
		nr, ok := SyscallNumber(tc.syscall)
		if !ok {
			continue
		}
		if got := evalFilter(t, filter, native, nr); got != tc.want {
			t.Errorf("strict profile decides %#x for %s, want %#x", got, tc.syscall, tc.want)
		}
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
