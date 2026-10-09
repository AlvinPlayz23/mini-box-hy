//go:build linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
)

// inspectCommand prints JSON details for containers or images.
func inspectCommand(env *Env, args []string) error {
	if len(args) == 0 {
		return usageError("usage: minibox inspect CONTAINER|IMAGE")
	}
	results := make([]any, 0, len(args))
	for _, name := range args {
		if id, err := env.Store.Resolve(name); err == nil {
			c, err := container.Load(env.Store, id)
			if err != nil {
				return err
			}
			cfg, err := runtime.LoadConfig(c.ConfigPath())
			if err != nil {
				return err
			}
			results = append(results, map[string]any{"container": c.State, "config": cfg})
			continue
		}
		rec, err := env.Images.ResolveRecord(name)
		if err != nil {
			return fmt.Errorf("no container or image matches %q", name)
		}
		results = append(results, map[string]any{"image": rec})
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

// stopCommand stops running containers.
func stopCommand(env *Env, args []string) error {
	fs := newFlagSet("stop")
	timeout := fs.Int("t", 10, "seconds to wait before SIGKILL")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError("usage: minibox stop [-t SECS] CONTAINER...")
	}
	for _, name := range rest {
		c, err := container.Load(env.Store, name)
		if err != nil {
			return err
		}
		if c.State.Status != container.StatusRunning {
			fmt.Fprintf(env.Stdout, "%s is not running\n", short(c.ID))
			continue
		}
		if err := container.Stop(env.Store, c, timeoutDuration(*timeout)); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s\n", short(c.ID))
	}
	return nil
}

// rmCommand removes containers.
func rmCommand(env *Env, args []string) error {
	fs := newFlagSet("rm")
	force := fs.Bool("f", false, "force removal of running containers")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError("usage: minibox rm [-f] CONTAINER...")
	}
	for _, name := range rest {
		c, err := container.Load(env.Store, name)
		if err != nil {
			return err
		}
		if err := container.Remove(env.Store, c, *force); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s\n", short(c.ID))
	}
	return nil
}

// systemCommand implements `minibox system prune`.
func systemCommand(env *Env, args []string) error {
	if len(args) == 0 || args[0] != "prune" {
		return usageError("usage: minibox system prune")
	}
	removed, err := env.Store.Prune()
	if err != nil {
		return err
	}
	for _, id := range removed {
		fmt.Fprintf(env.Stdout, "removed container %s\n", short(id))
	}
	blobs, err := env.Images.PruneBlobs()
	if err != nil {
		return err
	}
	if len(blobs) > 0 {
		fmt.Fprintf(env.Stdout, "removed %d unreferenced blobs\n", len(blobs))
	}
	// Cgroups of containers that are gone, plus stale overlay mounts.
	if err := container.PruneRuntimeState(env.Store, func(msg string) { fmt.Fprintln(env.Stdout, msg) }); err != nil {
		return err
	}
	// Named volumes nobody references, plus the layer cache of builds.
	if store, err := volumeStore(env); err == nil {
		if removed, err := store.Prune(volumesInUse(env)); err == nil {
			for _, name := range removed {
				fmt.Fprintf(env.Stdout, "removed volume %s\n", name)
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(env.Store.Root, "build", "cache")); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "removed the build layer cache")
	fmt.Fprintln(env.Stdout, "prune complete")
	return nil
}

// imageListJSON is a helper used by tests to decode `images --json`.
type imageListJSON = []*image.Record
