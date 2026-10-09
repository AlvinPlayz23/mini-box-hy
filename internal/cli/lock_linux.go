//go:build linux

package cli

import "runtime"

// runtimeLockOSThread pins the current goroutine to its thread, which is
// required before per-thread namespace operations.
func runtimeLockOSThread() { runtime.LockOSThread() }
