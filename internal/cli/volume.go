//go:build linux

package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/volume"
)

// volumeStore opens the named-volume store of the current state root.
func volumeStore(env *Env) (*volume.Store, error) {
	return volume.Open(env.Store.Root + "/volumes")
}

// volumesInUse collects the volume names referenced by existing containers.
func volumesInUse(env *Env) map[string]bool {
	ids, err := env.Store.List()
	if err != nil {
		return nil
	}
	var specs [][]string
	for _, id := range ids {
		c, err := container.Load(env.Store, id)
		if err != nil {
			continue
		}
		cfg, err := runtime.LoadConfig(c.ConfigPath())
		if err != nil {
			continue
		}
		var mounts []string
		for _, m := range cfg.Mounts {
			if m.Type == "bind" && volume.IsName(m.Source) {
				mounts = append(mounts, m.Source)
			}
		}
		specs = append(specs, mounts)
	}
	return volume.NamesInUse(specs)
}

// volumeCommand implements `minibox volume ...`.
func volumeCommand(env *Env, args []string) error {
	if len(args) == 0 {
		return usageError("usage: minibox volume create|ls|inspect|rm|prune [NAME]")
	}
	store, err := volumeStore(env)
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return usageError("usage: minibox volume create NAME")
		}
		v, err := store.Create(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, v.Name)
		return nil
	case "ls", "list":
		asJSON := false
		for _, a := range args[1:] {
			if a == "--json" {
				asJSON = true
			}
		}
		volumes, err := store.List()
		if err != nil {
			return err
		}
		if asJSON {
			enc := json.NewEncoder(env.Stdout)
			enc.SetIndent("", "  ")
			if volumes == nil {
				volumes = []*volume.Volume{}
			}
			return enc.Encode(volumes)
		}
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMOUNTPOINT\tCREATED")
		for _, v := range volumes {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Name, v.Mountpoint, humanAge(v.Created))
		}
		return tw.Flush()
	case "inspect":
		if len(args) < 2 {
			return usageError("usage: minibox volume inspect NAME")
		}
		v, err := store.Get(args[1])
		if err != nil {
			return err
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "rm", "remove":
		force := false
		var names []string
		for _, a := range args[1:] {
			if a == "-f" || a == "--force" {
				force = true
				continue
			}
			names = append(names, a)
		}
		if len(names) == 0 {
			return usageError("usage: minibox volume rm [-f] NAME...")
		}
		inUse := volumesInUse(env)
		for _, name := range names {
			if err := store.Remove(name, force, inUse); err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout, name)
		}
		return nil
	case "prune":
		removed, err := store.Prune(volumesInUse(env))
		if err != nil {
			return err
		}
		for _, name := range removed {
			fmt.Fprintf(env.Stdout, "removed volume %s\n", name)
		}
		return nil
	default:
		return usageError("unknown volume subcommand %q: use create, ls, inspect, rm or prune", args[0])
	}
}
