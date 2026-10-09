//go:build linux

// Package cli implements the minibox command line: argument parsing, command
// dispatch and the hidden helper subcommands used by the runtime.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// envStoreContext returns the context used by store-level commands.
func envStoreContext() context.Context { return context.Background() }

// timeoutDuration converts a seconds flag into a duration.
func timeoutDuration(seconds int) time.Duration { return time.Duration(seconds) * time.Second }

// Version is the minibox version string.
const Version = "0.1.0"

// Env carries the process-wide dependencies of a command.
type Env struct {
	Stdout, Stderr io.Writer
	Stdin          *os.File
	StdoutFile     *os.File
	StderrFile     *os.File
	Store          *container.Store
	Images         *image.Store
	Progress       func(string)
}

// NewEnv opens the stores for the current MINIBOX_ROOT.
func NewEnv() (*Env, error) {
	initNetworkWiring()
	store, err := container.Open()
	if err != nil {
		return nil, err
	}
	return &Env{
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Stdin:      os.Stdin,
		StdoutFile: os.Stdout,
		StderrFile: os.Stderr,
		Store:      store,
		Images:     image.OpenStore(store),
	}, nil
}

// Run dispatches a minibox invocation and returns the process exit code.
func Run(args []string) int {
	env, err := NewEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "minibox: %v\n", err)
		return 1
	}
	env.Progress = func(msg string) { fmt.Fprintf(env.Stderr, "minibox: %s\n", msg) }
	return RunWithEnv(env, args)
}

// RunWithEnv dispatches a minibox invocation with an explicit environment; it
// exists so tests can capture output.
func RunWithEnv(env *Env, args []string) int {
	if len(args) == 0 {
		printUsage(env.Stdout)
		return 0
	}
	cmd, rest := args[0], args[1:]

	var runErr error
	switch cmd {
	case "run":
		return runCommand(env, rest)
	case "run-raw":
		return runRawCommand(env, rest)
	case "ps":
		runErr = psCommand(env, rest)
	case "images":
		runErr = imagesCommand(env, rest)
	case "pull":
		runErr = pullCommand(env, rest)
	case "load":
		runErr = loadCommand(env, rest)
	case "rmi":
		runErr = rmiCommand(env, rest)
	case "exec":
		return execCommand(env, rest)
	case "logs":
		runErr = logsCommand(env, rest)
	case "stop":
		runErr = stopCommand(env, rest)
	case "rm":
		runErr = rmCommand(env, rest)
	case "inspect":
		runErr = inspectCommand(env, rest)
	case "system":
		runErr = systemCommand(env, rest)
	case "version", "--version", "-v":
		fmt.Fprintf(env.Stdout, "minibox %s\n", Version)
	case "help", "--help", "-h":
		printUsage(env.Stdout)
	// Hidden commands used by the runtime.
	case "init":
		if err := runtime.Init(); err != nil {
			fmt.Fprintf(os.Stderr, "minibox init: %v\n", err)
			return 126
		}
		return 0
	case "minibox-init":
		return runtime.Reap(rest)
	case "shim":
		return shimCommand(env, rest)
	case "exec-child":
		return execChildCommand(env, rest)
	default:
		fmt.Fprintf(env.Stderr, "minibox: unknown command %q\n\n", cmd)
		printUsage(env.Stderr)
		return 1
	}
	if runErr != nil {
		return reportError(env, runErr)
	}
	return 0
}

// reportError prints an error and picks an exit code.
func reportError(env *Env, err error) int {
	var exitErr *ExitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	fmt.Fprintf(env.Stderr, "minibox: %v\n", err)
	return 1
}

// ExitCodeError carries an explicit process exit code.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// usageError marks a bad invocation so Run can print usage.
func usageError(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// stringList collects repeatable string flags.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// newFlagSet builds a flag set with minibox's error style.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses flags and turns flag errors into usage errors.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usageError("%v (see `minibox help`)", err)
	}
	return nil
}

// printUsage writes the command overview.
func printUsage(w io.Writer) {
	fmt.Fprint(w, `minibox - a fast, daemonless container runtime

Usage:
  minibox run [flags] IMAGE [CMD...]     run a container (foreground, or -d detached)
  minibox ps [-a] [--json]               list containers
  minibox images [--json]                list local images
  minibox pull IMAGE                     download an image from a registry
  minibox load [-t NAME:TAG] FILE        import a rootfs or docker-save archive
  minibox rmi IMAGE...                   remove images
  minibox exec [-it] CONTAINER CMD...    run a command in a running container
  minibox logs [-f] CONTAINER            print a container's log
  minibox stop [-t SECS] CONTAINER...    stop containers (SIGTERM, then SIGKILL)
  minibox rm [-f] CONTAINER...           remove containers
  minibox inspect CONTAINER|IMAGE        print JSON details
  minibox system prune                   remove stopped containers and unused data
  minibox version                        print the version

run flags:
  -d, --detach           run in the background
      --rm               remove the container when it exits
      --name NAME        assign a name
  -it                    allocate a tty and keep stdin open
  -e, --env KEY=VAL      set an environment variable (repeatable)
  -v, --volume SRC:DST   bind mount (add :ro for read-only, tmpfs:DST for tmpfs)
  -w, --workdir DIR      working directory
  -u, --user USER[:GRP]  run as this user
  -p, --publish P:C      publish a container port on the host (repeatable)
      --network MODE     none, host, bridge or pasta
      --hostname NAME    container hostname
      --memory SIZE      memory limit, e.g. 50m
      --memory-swap SIZE total memory+swap limit
      --cpus N           cpu limit, e.g. 0.5
      --pids-limit N     maximum number of processes
      --read-only        mount the root filesystem read-only
      --init             run a tiny init inside the container
      --cap-add CAP      add a capability (repeatable, or ALL)
      --cap-drop CAP     drop a capability (repeatable, or ALL)
      --seccomp PROFILE  default or unconfined
      --platform OS/ARCH select the image platform

State lives under $MINIBOX_ROOT (default /var/lib/minibox for root,
~/.local/share/minibox otherwise).
`)
}

// parseIntFlag parses a positive integer flag value.
func parseIntFlag(value, name string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, usageError("invalid %s value %q: expected a number", name, value)
	}
	return n, nil
}
