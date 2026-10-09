//go:build linux

package cli

import "runtime/debug"

// debugSetGCPercent and debugSetMemoryLimit are thin wrappers so the tuning
// knobs stay in one place and are easy to change after profiling.
func debugSetGCPercent(percent int)   { debug.SetGCPercent(percent) }
func debugSetMemoryLimit(bytes int64) { debug.SetMemoryLimit(bytes) }
