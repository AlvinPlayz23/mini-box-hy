//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/compose"
	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/volume"
)

// composeFiles are tried in order when -f is not given.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml", "minibox.yaml"}

// findComposeFile returns the compose file to use.
func findComposeFile(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("compose file %s: %w", explicit, err)
		}
		return explicit, nil
	}
	for _, name := range composeFiles {
		if _, err := os.Stat(name); err == nil {
			abs, err := filepath.Abs(name)
			if err != nil {
				return "", err
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("no compose file found (looked for %s): pass -f FILE", strings.Join(composeFiles, ", "))
}

// projectContainerName mirrors compose's naming: <project>_<service>_1.
func projectContainerName(project, service string) string {
	return sanitizeName(project) + "_" + sanitizeName(service) + "_1"
}

// sanitizeName keeps the characters compose allows in project and service
// names, replacing everything else with an underscore.
func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}

// upCommand implements `minibox up`.
func upCommand(env *Env, args []string) error {
	fs := newFlagSet("up")
	file := fs.String("f", "", "compose file")
	detach := fs.Bool("d", false, "detached")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path, err := findComposeFile(*file)
	if err != nil {
		return err
	}
	project, err := compose.Load(path)
	if err != nil {
		return err
	}
	order, err := project.StartOrder()
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, name := range fs.Args() {
		if _, ok := project.Service(name); !ok {
			return usageError("no service named %q in %s", name, path)
		}
		selected[name] = true
	}

	// Start dependencies first; stop what we started if anything fails.
	var started []string
	for _, name := range order {
		if len(selected) > 0 && !selected[name] {
			continue
		}
		svc, _ := project.Service(name)
		id, err := startService(env, project, svc)
		if err != nil {
			stopProjectContainers(env, project, started, 5*time.Second)
			return err
		}
		started = append(started, id)
		fmt.Fprintf(env.Stdout, "%s started (%s)\n", projectContainerName(project.Name, svc.Name), short(id))
	}

	if *detach {
		return nil
	}
	// Foreground mode follows the logs of every container, like compose, and
	// stops the project on Ctrl-C.
	fmt.Fprintln(env.Stdout, "attaching to the project (Ctrl-C stops it)")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		<-sig
		fmt.Fprintln(env.Stdout, "\nstopping the project")
		stopProjectContainers(env, project, started, 10*time.Second)
		close(done)
	}()
	followAll(env, started)
	select {
	case <-done:
	default:
		stopProjectContainers(env, project, started, 10*time.Second)
	}
	return nil
}

// startService creates and starts one compose service.
func startService(env *Env, project *compose.Project, svc *compose.Service) (string, error) {
	imageRef := svc.Image
	if imageRef == "" {
		built, err := buildServiceImage(env, project, svc)
		if err != nil {
			return "", err
		}
		imageRef = built
	}
	ref, err := parseImageRef(imageRef)
	if err != nil {
		return "", err
	}
	rec, err := ensureImage(context.Background(), env, ref, "")
	if err != nil {
		return "", fmt.Errorf("service %s: %w", svc.Name, err)
	}
	args, err := resolveCommand(rec.Config.Config, svc.Command)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", svc.Name, err)
	}
	entrypoint := svc.Entrypoint
	if len(entrypoint) > 0 {
		args = append(entrypoint, svc.Command...)
		if len(svc.Command) == 0 {
			args = append(entrypoint, rec.Config.Config.Cmd...)
		}
	}
	cfg := &runtime.Config{
		Image:    rec.Ref,
		Args:     args,
		Env:      mergeEnv(rec.Config.Config.Env, svc.Environment),
		Cwd:      firstNonEmpty(svc.WorkingDir, rec.Config.Config.WorkingDir, "/"),
		User:     firstNonEmpty(svc.User, rec.Config.Config.User),
		Hostname: firstNonEmpty(svc.Hostname, svc.Name),
		Tty:      svc.Tty,
		Detach:   true,
		Init:     svc.Init,
		Restart:  svc.Restart,
		Rootfs:   runtime.Rootfs{Lower: []string{env.Images.LayerDir(rec.ChainID)}, Overlay: true},
		Created:  time.Now().UTC(),
	}
	cfg.Network.Hostname = cfg.Hostname
	cfg.Network.Mode = svc.NetworkMode
	if cfg.Network.Mode == "" {
		if os.Geteuid() == 0 {
			cfg.Network.Mode = "bridge"
		} else {
			cfg.Network.Mode = "pasta"
		}
	}
	if cfg.Network.Mode != "none" {
		cfg.Network.DNS = hostDNS()
	}
	for _, spec := range svc.Ports {
		m, err := parsePortSpec(spec)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		cfg.Network.Ports = append(cfg.Network.Ports, m)
	}
	if svc.Memory != "" {
		n, err := parseBytesFlag(svc.Memory)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		cfg.Resources.MemoryBytes = n
	}
	if svc.CPUs != "" {
		cfg.Resources.CPUs = svc.CPUs
	}
	cfg.Resources.PidsLimit = svc.PidsLimit
	caps, err := resolveCaps(svc.CapAdd, svc.CapDrop)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", svc.Name, err)
	}
	cfg.Security = runtime.Security{
		Capabilities: caps,
		CapAdd:       svc.CapAdd,
		CapDrop:      svc.CapDrop,
		Seccomp:      "default",
		NoNewPrivs:   true,
		Rootless:     os.Geteuid() != 0,
	}
	if svc.HealthCheck != nil {
		cfg.HealthCheck = &runtime.HealthCheck{
			Test:        svc.HealthCheck.Test,
			Interval:    svc.HealthCheck.Interval,
			Timeout:     svc.HealthCheck.Timeout,
			Retries:     svc.HealthCheck.Retries,
			StartPeriod: svc.HealthCheck.StartPeriod,
		}
		cfg.HealthCheck.Normalize()
	}
	opts := &runOptions{}
	for _, spec := range svc.Volumes {
		m, err := parseVolume(spec, env)
		if err != nil {
			return "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		cfg.Mounts = append(cfg.Mounts, m)
		opts.volumes = append(opts.volumes, spec)
	}

	name := projectContainerName(project.Name, svc.Name)
	c, err := container.Create(env.Store, cfg, name)
	if err != nil {
		return "", err
	}
	if err := startDetached(env, c.ID, cfg); err != nil {
		cleanupFailedStart(env, c.ID)
		return "", err
	}
	return c.ID, nil
}

// downCommand implements `minibox down`.
func downCommand(env *Env, args []string) error {
	fs := newFlagSet("down")
	file := fs.String("f", "", "compose file")
	removeVolumes := fs.Bool("volumes", false, "remove named volumes declared by the project")
	fs.BoolVar(removeVolumes, "v", false, "")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path, err := findComposeFile(*file)
	if err != nil {
		return err
	}
	project, err := compose.Load(path)
	if err != nil {
		return err
	}
	ids := projectContainerIDs(env, project)
	if len(ids) == 0 {
		fmt.Fprintln(env.Stdout, "no containers for this project")
	} else {
		for _, id := range ids {
			c, err := container.Load(env.Store, id)
			if err != nil {
				continue
			}
			if err := container.Remove(env.Store, c, true); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "removed %s\n", short(id))
		}
	}
	if *removeVolumes {
		store, err := volumeStore(env)
		if err != nil {
			return err
		}
		removed, err := store.Prune(volumesInUse(env))
		if err != nil {
			return err
		}
		for _, name := range removed {
			fmt.Fprintf(env.Stdout, "removed volume %s\n", name)
		}
	}
	return nil
}

// projectContainerIDs finds the containers belonging to a compose project.
func projectContainerIDs(env *Env, project *compose.Project) []string {
	ids, err := env.Store.List()
	if err != nil {
		return nil
	}
	prefix := sanitizeName(project.Name) + "_"
	var out []string
	for _, id := range ids {
		c, err := container.Load(env.Store, id)
		if err != nil {
			continue
		}
		if strings.HasPrefix(c.State.Name, prefix) {
			out = append(out, id)
		}
	}
	return out
}

// stopProjectContainers stops the containers a failed or interrupted `up`
// started.
func stopProjectContainers(env *Env, project *compose.Project, ids []string, timeout time.Duration) {
	for _, id := range ids {
		c, err := container.Load(env.Store, id)
		if err != nil {
			continue
		}
		_ = container.Stop(env.Store, c, timeout)
	}
}

// followAll streams the logs of the given containers until they all exit.
func followAll(env *Env, ids []string) {
	for _, id := range ids {
		c, err := container.Load(env.Store, id)
		if err != nil {
			continue
		}
		f, err := os.Open(c.LogPath())
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(c.LogPath())
		if len(data) > 0 {
			fmt.Fprintf(env.Stdout, "--- %s ---\n%s", c.State.Name, data)
		}
		f.Close()
	}
}

// buildServiceImage builds a service that declares build: instead of image:.
func buildServiceImage(env *Env, project *compose.Project, svc *compose.Service) (string, error) {
	tag := project.Name + "-" + svc.Name + ":latest"
	ref, err := buildImage(context.Background(), env, svc.Build, svc.Dockerfile, tag)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", svc.Name, err)
	}
	return ref, nil
}

var _ = volume.IsName
