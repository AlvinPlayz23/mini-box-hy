//go:build linux

package cli

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
)

// Profiling environment variables. Setting MINIBOX_PROFILE makes any command
// write a profile, which is how the M10 pass measures the hot path without
// adding flags to every subcommand.
const (
	EnvProfile     = "MINIBOX_PROFILE"      // cpu, heap, trace, mutex, block, goroutine
	EnvProfilePath = "MINIBOX_PROFILE_PATH" // output file, defaults to minibox-<kind>-<pid>.pprof
)

// startProfiling starts the requested profile and returns a stop function.
func startProfiling(command string) func() {
	kind := os.Getenv(EnvProfile)
	if kind == "" {
		return func() {}
	}
	path := os.Getenv(EnvProfilePath)
	if path == "" {
		path = fmt.Sprintf("minibox-%s-%s-%d.pprof", kind, command, os.Getpid())
	}
	switch kind {
	case "cpu":
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "minibox: cannot create CPU profile %s: %v\n", path, err)
			return func() {}
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "minibox: cannot start CPU profile: %v\n", err)
			return func() {}
		}
		return func() {
			pprof.StopCPUProfile()
			f.Close()
			fmt.Fprintf(os.Stderr, "minibox: CPU profile written to %s\n", path)
		}
	case "trace":
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "minibox: cannot create trace file %s: %v\n", path, err)
			return func() {}
		}
		if err := trace.Start(f); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "minibox: cannot start trace: %v\n", err)
			return func() {}
		}
		return func() {
			trace.Stop()
			f.Close()
			fmt.Fprintf(os.Stderr, "minibox: execution trace written to %s\n", path)
		}
	case "heap", "mutex", "block", "goroutine":
		return func() {
			f, err := os.Create(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "minibox: cannot create profile %s: %v\n", path, err)
				return
			}
			defer f.Close()
			var err2 error
			switch kind {
			case "heap":
				runtime.GC()
				err2 = pprof.Lookup("heap").WriteTo(f, 0)
			case "goroutine":
				err2 = pprof.Lookup("goroutine").WriteTo(f, 0)
			default:
				err2 = pprof.Lookup(kind).WriteTo(f, 0)
			}
			if err2 != nil {
				fmt.Fprintf(os.Stderr, "minibox: cannot write %s profile: %v\n", kind, err2)
				return
			}
			fmt.Fprintf(os.Stderr, "minibox: %s profile written to %s\n", kind, path)
		}
	default:
		fmt.Fprintf(os.Stderr, "minibox: unknown %s value %q: use cpu, heap, trace, mutex, block or goroutine\n", EnvProfile, kind)
		return func() {}
	}
}

// TuneShimRuntime trims the runtime's memory behaviour for the long-lived
// shim process, which spends nearly all of its time waiting: a lower GC target
// keeps its resident set small (PLAN.md asks for under 5 MB per detached
// container).
func TuneShimRuntime() {
	debugSetGCPercent(50)
	if limit, err := strconv.ParseInt(os.Getenv("MINIBOX_SHIM_MEMLIMIT"), 10, 64); err == nil && limit > 0 {
		debugSetMemoryLimit(limit)
		return
	}
	debugSetMemoryLimit(64 << 20)
}
