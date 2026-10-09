//go:build linux

package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/container"
)

// psCommand lists containers.
func psCommand(env *Env, args []string) error {
	fs := newFlagSet("ps")
	all := fs.Bool("a", false, "show all containers")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ids, err := env.Store.List()
	if err != nil {
		return err
	}
	var states []*container.State
	for _, id := range ids {
		c, err := container.Load(env.Store, id)
		if err != nil {
			continue
		}
		if !*all && c.State.Status != container.StatusRunning {
			continue
		}
		states = append(states, c.State)
	}
	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if states == nil {
			states = []*container.State{}
		}
		return enc.Encode(states)
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTAINER ID\tNAME\tIMAGE\tCOMMAND\tSTATUS\tHEALTH\tEXIT\tCREATED")
	for _, st := range states {
		name := st.Name
		if name == "" {
			name = "-"
		}
		exit := "-"
		if st.Status == container.StatusExited {
			exit = fmt.Sprintf("%d", st.ExitCode)
		}
		health := st.Health
		if health == "" {
			health = "-"
		}
		status := string(st.Status)
		if st.Restarts > 0 {
			status = fmt.Sprintf("%s (%d restarts)", status, st.Restarts)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			short(st.ID), name, st.Image, truncate(st.Command, 40), status, health, exit, humanAge(st.CreatedAt))
	}
	return tw.Flush()
}

// short renders a container id prefix.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// truncate shortens a string for table output.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// humanAge renders a timestamp as an age.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
