//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/AlvinPlayz23/minibox/internal/api"
	"github.com/AlvinPlayz23/minibox/internal/daemon"
)

// serveCommand implements `minibox serve`: a Docker-compatible API on a socket.
func serveCommand(env *Env, args []string) error {
	fs := newFlagSet("serve")
	socket := fs.String("socket", "/run/minibox/minibox.sock", "unix socket path")
	tcp := fs.String("tcp", "", "optional TCP address, for example 127.0.0.1:2375")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *socket == "" && *tcp == "" {
		return usageError("serve needs at least one listener: --socket PATH or --tcp ADDR")
	}
	backend, err := daemon.New(env.Store.Root)
	if err != nil {
		return err
	}
	backend.Progress = env.Progress
	backend.Network = networkHook()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *socket != "" {
		fmt.Fprintf(env.Stderr, "minibox: serving the Docker API on unix://%s\n", *socket)
	}
	if *tcp != "" {
		fmt.Fprintf(env.Stderr, "minibox: serving the Docker API on tcp://%s\n", *tcp)
	}
	if err := api.Serve(ctx, *socket, *tcp, backend); err != nil {
		return fmt.Errorf("serve the Docker API: %w", err)
	}
	os.Stderr.WriteString("minibox: api server stopped\n")
	return nil
}
