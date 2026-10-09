//go:build linux

package security

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ApplySeccomp installs the filter on the calling thread. It sets
// no_new_privs first, as the kernel requires for unprivileged processes and as
// a hardening measure in general.
func ApplySeccomp(filter []unix.SockFilter) error {
	if len(filter) == 0 {
		return nil
	}
	if err := SetNoNewPrivs(); err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER): %w", err)
	}
	return nil
}
