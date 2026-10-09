//go:build linux

package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

// logsCommand prints (and optionally follows) a container's log.
func logsCommand(env *Env, args []string) error {
	fs := newFlagSet("logs")
	follow := fs.Bool("f", false, "follow")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageError("usage: minibox logs [-f] CONTAINER")
	}
	c, err := container.Load(env.Store, rest[0])
	if err != nil {
		return err
	}
	f, err := os.Open(c.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("container %s has no log yet", c.ID[:12])
		}
		return err
	}
	defer f.Close()
	if _, err := io.Copy(env.Stdout, f); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	// Poll for growth: no inotify dependency, works on every filesystem.
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := env.Stdout.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		live, lerr := container.Load(env.Store, c.ID)
		if lerr == nil && live.State.Status != container.StatusRunning {
			// Flush anything written after the last read, then stop.
			if n, _ := f.Read(buf); n > 0 {
				_, _ = env.Stdout.Write(buf[:n])
			}
			return nil
		}
	}
}
