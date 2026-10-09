//go:build linux

package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/AlvinPlayz23/minibox/internal/image"
)

// imagesCommand lists stored images.
func imagesCommand(env *Env, args []string) error {
	fs := newFlagSet("images")
	asJSON := fs.Bool("json", false, "output JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	records, err := env.Images.ListRecords()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if records == nil {
			records = []*image.Record{}
		}
		return enc.Encode(records)
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tTAG\tDIGEST\tSIZE\tCREATED")
	for _, rec := range records {
		ref, err := image.ParseReference(rec.Ref)
		if err != nil {
			continue
		}
		tag := ref.Tag
		if tag == "" {
			tag = "<none>"
		}
		name := ref.Registry + "/" + ref.Repository
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, tag, shortDigest(rec.Digest), humanSize(rec.Size), humanAge(rec.Created))
	}
	return tw.Flush()
}

// pullCommand downloads an image.
func pullCommand(env *Env, args []string) error {
	fs := newFlagSet("pull")
	platform := fs.String("platform", "", "select the image platform")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageError("usage: minibox pull IMAGE")
	}
	ref, err := image.ParseReference(rest[0])
	if err != nil {
		return err
	}
	opts := image.PullOptions{Progress: env.Progress}
	if *platform != "" {
		p, err := image.ParsePlatform(*platform)
		if err != nil {
			return err
		}
		opts.Platform = p
	}
	rec, err := image.Pull(envStoreContext(), env.Images, ref, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s\n", rec.Ref)
	return nil
}

// loadCommand imports a local archive.
func loadCommand(env *Env, args []string) error {
	fs := newFlagSet("load")
	tag := fs.String("t", "", "tag for the imported image")
	fs.StringVar(tag, "tag", "", "")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageError("usage: minibox load [-t NAME:TAG] FILE")
	}
	rec, err := image.LoadArchive(envStoreContext(), env.Images, rest[0], image.LoadOptions{
		Tag:      *tag,
		Progress: env.Progress,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "loaded %s\n", rec.Ref)
	return nil
}

// rmiCommand removes images.
func rmiCommand(env *Env, args []string) error {
	if len(args) == 0 {
		return usageError("usage: minibox rmi IMAGE...")
	}
	for _, name := range args {
		chain, err := env.Images.RemoveRecord(name)
		if err != nil {
			return err
		}
		if chain != "" {
			fmt.Fprintf(env.Stdout, "untagged %s (removed layer chain %s)\n", name, chain[:12])
		} else {
			fmt.Fprintf(env.Stdout, "untagged %s\n", name)
		}
	}
	if _, err := env.Images.PruneBlobs(); err != nil {
		return err
	}
	return nil
}

// shortDigest renders a digest prefix.
func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

// humanSize renders a byte count.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
