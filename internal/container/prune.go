//go:build linux

package container

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/storage"
)

// PruneRuntimeState cleans up leftovers that the normal lifecycle may have
// missed: overlay mounts of dead containers, their cgroups, and network state.
func PruneRuntimeState(store *Store, log func(string)) error {
	if log == nil {
		log = func(string) {}
	}
	ids, err := store.List()
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, id := range ids {
		c, err := Load(store, id)
		if err != nil {
			continue
		}
		if c.State.Status == StatusRunning && processAlive(c.State.PID) {
			live[id] = true
			continue
		}
		cfg, err := runtime.LoadConfig(c.ConfigPath())
		if err == nil {
			rootfs := storage.RootfsPaths{
				Merged:  cfg.Rootfs.Merged,
				Upper:   cfg.Rootfs.Upper,
				Work:    cfg.Rootfs.Work,
				Overlay: cfg.Rootfs.Overlay,
			}
			if err := storage.TeardownRootfs(rootfs); err != nil {
				log(fmt.Sprintf("warning: %v", err))
			}
			if cfg.Network.Mode != "" {
				_ = teardownNetwork(cfg.Network, c.Dir)
			}
		}
	}

	// Cgroups without a live container.
	if mgr, err := cgroup.Detect(); err == nil {
		names, err := mgr.List()
		if err == nil {
			for _, name := range names {
				if live[name] {
					continue
				}
				cg, err := mgr.Open(name)
				if err != nil {
					continue
				}
				_ = cg.Kill()
				if err := cg.Remove(); err == nil {
					log(fmt.Sprintf("removed cgroup %s", name))
				}
			}
		}
	}

	// Overlay mounts under the state root that belong to nobody.
	if err := pruneMounts(store, log); err != nil {
		return err
	}
	// Stale temp directories from interrupted unpacks.
	if err := pruneTempDirs(store, log); err != nil {
		return err
	}
	return nil
}

// pruneMounts unmounts overlay filesystems under the state root that are not
// part of a running container.
func pruneMounts(store *Store, log func(string)) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	mounts, err := storage.ParseMountInfo(f)
	if err != nil {
		return err
	}
	for _, m := range storage.MountedPoints(mounts, store.ContainersDir()) {
		if err := storage.UnmountOverlay(m); err == nil {
			log(fmt.Sprintf("unmounted %s", m))
		}
	}
	return nil
}

// pruneTempDirs removes interrupted unpack directories.
func pruneTempDirs(store *Store, log func(string)) error {
	entries, err := os.ReadDir(store.LayersDir())
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".tmp-unpack-") {
			continue
		}
		path := filepath.Join(store.LayersDir(), e.Name())
		if err := os.RemoveAll(path); err == nil {
			log(fmt.Sprintf("removed %s", path))
		}
	}
	return nil
}
