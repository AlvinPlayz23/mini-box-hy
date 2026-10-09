//go:build linux

package cli

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlvinPlayz23/minibox/internal/build"
	"github.com/AlvinPlayz23/minibox/internal/container"
	"github.com/AlvinPlayz23/minibox/internal/image"
	"github.com/AlvinPlayz23/minibox/internal/runtime"
	"github.com/AlvinPlayz23/minibox/internal/storage"
)

// buildCacheEntry is the layer cache record for one instruction prefix.
type buildCacheEntry struct {
	// DeltaDigest is the blob holding every filesystem change up to this
	// instruction, relative to the stage's base image.
	DeltaDigest string `json:"delta_digest"`
	// DeltaSize is the compressed size of that blob.
	DeltaSize int64 `json:"delta_size"`
	// Config is the image configuration at this point in the build.
	Config image.ConfigFile `json:"config"`
}

// buildImage builds contextDir/dockerfile and tags the result as tag, returning
// the stored reference. It implements the M9 Dockerfile subset with layer
// caching keyed by the instruction prefix and the contents of any COPY inputs.
func buildImage(ctx context.Context, env *Env, contextDir, dockerfile, tag string) (string, error) {
	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(absContext); err != nil || !info.IsDir() {
		return "", fmt.Errorf("build context %s is not a directory", absContext)
	}
	dockerfilePath := dockerfile
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(absContext, dockerfilePath)
	}
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", dockerfilePath, err)
	}
	stages, err := build.ParseDockerfile(f)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("%s: %w", dockerfilePath, err)
	}
	if len(stages) == 0 {
		return "", fmt.Errorf("%s contains no FROM instruction", dockerfilePath)
	}

	// Only the target stage is built; COPY --from=<stage> is rejected with a
	// message that says so (see docs/DECISIONS.md).
	var target *build.Stage
	for i := range stages {
		target = &stages[i]
	}

	baseRef, err := image.ParseReference(target.Base)
	if err != nil {
		return "", fmt.Errorf("stage %d: %w", target.Index, err)
	}
	baseRec, err := ensureImage(ctx, env, baseRef, "")
	if err != nil {
		return "", err
	}
	env.Progress(fmt.Sprintf("building from %s", baseRec.Ref))

	b := &builder{
		env:        env,
		contextDir: absContext,
		stages:     stages,
		baseLayers: layerDigests(baseRec),
		baseKey:    baseRec.ChainID,
		config:     baseRec.Config,
	}
	return b.buildStage(ctx, target, tag)
}

// builder carries the state of one image build.
type builder struct {
	env        *Env
	contextDir string
	stages     []build.Stage
	baseLayers []image.LayerRef
	baseKey    string
	config     image.ConfigFile

	dir    string // build workspace: upper, work, merged
	merged string
	upper  string
	work   string

	delta   string // digest of the cumulative delta layer
	deltaSz int64
	vars    map[string]string
}

// layerDigests returns the layer references of an image record.
func layerDigests(rec *image.Record) []image.LayerRef {
	out := make([]image.LayerRef, len(rec.Layers))
	copy(out, rec.Layers)
	return out
}

// buildStage executes a stage's instructions and stores the resulting image.
func (b *builder) buildStage(ctx context.Context, stage *build.Stage, tag string) (string, error) {
	baseDir := b.env.Images.LayerDir(imageChainID(b.baseLayers))
	if err := b.prepareWorkspace(baseDir); err != nil {
		return "", err
	}
	defer b.cleanup()

	b.vars = build.VarsFromInstructions(b.stages, nil)
	workdir := b.config.Config.WorkingDir
	if workdir == "" {
		workdir = "/"
	}

	// Resume from the longest cached prefix, so an unchanged Dockerfile and
	// unchanged COPY inputs skip execution entirely.
	start := 0
	prefix := b.baseKey
	prefixKeys := make([]string, len(stage.Instructions))
	for i, ins := range stage.Instructions {
		ctxHash := ""
		if strings.EqualFold(ins.Cmd, "COPY") || strings.EqualFold(ins.Cmd, "ADD") {
			patterns, perr := copyPatterns(ins, b.vars)
			if perr != nil {
				return "", perr
			}
			ctxHash, _ = build.ContextHash(b.contextDir, patterns)
		}
		prefix = build.CacheKey(prefix, ins, ctxHash)
		prefixKeys[i] = prefix
	}
	for i := range stage.Instructions {
		entry, ok := b.readCache(prefixKeys[i])
		if !ok {
			break
		}
		start = i + 1
		b.delta = entry.DeltaDigest
		b.deltaSz = entry.DeltaSize
		b.config = entry.Config
	}
	if start > 0 {
		b.env.Progress(fmt.Sprintf("cache hit: reusing %d of %d instructions", start, len(stage.Instructions)))
		if b.delta != "" {
			layers := append(layerDigests(&image.Record{Layers: b.baseLayers}), image.LayerRef{Digest: b.delta, Size: b.deltaSz, MediaType: image.MediaTypeLayerGzip})
			resume := &image.Record{
				Ref:        tag,
				Repository: "minibox-cache",
				ChainID:    imageChainID(layers),
				Layers:     layers,
				Config:     b.config,
			}
			if err := b.env.Images.RegisterRecord(resume, nil); err != nil {
				return "", err
			}
			if err := b.remount(b.env.Images.LayerDir(resume.ChainID)); err != nil {
				return "", err
			}
		}
	}

	for i := start; i < len(stage.Instructions); i++ {
		ins := stage.Instructions[i]
		if err := b.runInstruction(ctx, ins, &workdir); err != nil {
			return "", fmt.Errorf("%s:%d: %w", "Dockerfile", ins.Line, err)
		}
		if b.delta != "" {
			b.writeCache(prefixKeys[i])
		}
	}

	rec := &image.Record{
		Ref:        tag,
		Registry:   "docker.io",
		Repository: "library/" + sanitizeName(tagName(tag)),
		Tag:        tagPart(tag),
		Created:    time.Now().UTC(),
		Config:     b.config,
	}
	layers := layerDigests(&image.Record{Layers: b.baseLayers})
	if b.delta != "" {
		layers = append(layers, image.LayerRef{Digest: b.delta, Size: b.deltaSz, MediaType: image.MediaTypeLayerGzip})
	}
	rec.Layers = layers
	rec.ChainID = imageChainID(layers)
	rec.Digest = rec.ChainID
	rec.Size = 0
	for _, l := range layers {
		rec.Size += l.Size
	}
	ref, err := image.ParseReference(tag)
	if err != nil {
		return "", err
	}
	rec.Ref = ref.Name()
	rec.Registry = ref.Registry
	rec.Repository = ref.Repository
	rec.Tag = ref.Tag
	if err := b.env.Images.RegisterRecord(rec, func(msg string) { b.env.Progress(msg) }); err != nil {
		return "", err
	}
	b.env.Progress(fmt.Sprintf("built %s", ref.Familiar()))
	return ref.Name(), nil
}

// prepareWorkspace creates the build directory and mounts the overlay that all
// steps share, so writes from one RUN are visible to the next.
func (b *builder) prepareWorkspace(lowerDir string) error {
	dir, err := os.MkdirTemp(filepath.Join(b.env.Store.Root, "build"), "step-")
	if err != nil {
		if mkErr := os.MkdirAll(filepath.Join(b.env.Store.Root, "build"), 0o700); mkErr != nil {
			return mkErr
		}
		dir, err = os.MkdirTemp(filepath.Join(b.env.Store.Root, "build"), "step-")
		if err != nil {
			return err
		}
	}
	b.dir = dir
	b.merged = filepath.Join(dir, "merged")
	b.upper = filepath.Join(dir, "upper")
	b.work = filepath.Join(dir, "work")
	for _, d := range []string{b.merged, b.upper, b.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return storage.MountOverlay([]string{lowerDir}, b.upper, b.work, b.merged, os.Geteuid() != 0)
}

// remount switches the workspace to a different lower layer, which is how a
// cached prefix is resumed.
func (b *builder) remount(lowerDir string) error {
	if err := storage.UnmountOverlay(b.merged); err != nil {
		return err
	}
	return storage.MountOverlay([]string{lowerDir}, b.upper, b.work, b.merged, os.Geteuid() != 0)
}

// cleanup unmounts and removes the build workspace.
func (b *builder) cleanup() {
	if b.merged == "" {
		return
	}
	_ = storage.UnmountOverlay(b.merged)
	_ = os.RemoveAll(b.dir)
}

// runInstruction executes one Dockerfile instruction.
func (b *builder) runInstruction(ctx context.Context, ins build.Instruction, workdir *string) error {
	switch ins.Cmd {
	case "RUN":
		argv, err := runArgv(ins, b.vars)
		if err != nil {
			return err
		}
		return b.runStep(ctx, argv, *workdir)
	case "COPY", "ADD":
		return b.copyStep(ins)
	case "ENV":
		return b.envStep(ins)
	case "ARG":
		return nil
	case "WORKDIR":
		dir, err := build.ExpandArgs(ins.Args, b.vars)
		if err != nil {
			return err
		}
		dir = strings.TrimSpace(dir)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(*workdir, dir)
		}
		if err := os.MkdirAll(filepath.Join(b.merged, filepath.Clean("/"+dir)), 0o755); err != nil {
			return err
		}
		*workdir = dir
		b.config.Config.WorkingDir = dir
		return nil
	case "CMD":
		argv, err := execFormArgv(ins, b.vars)
		if err != nil {
			return err
		}
		b.config.Config.Cmd = argv
		return nil
	case "ENTRYPOINT":
		argv, err := execFormArgv(ins, b.vars)
		if err != nil {
			return err
		}
		b.config.Config.Entrypoint = argv
		return nil
	case "USER":
		user, err := build.ExpandArgs(ins.Args, b.vars)
		if err != nil {
			return err
		}
		b.config.Config.User = strings.TrimSpace(user)
		return nil
	case "LABEL":
		labels := parseKeyValues(ins.Args)
		if b.config.Config.Labels == nil {
			b.config.Config.Labels = map[string]string{}
		}
		for k, v := range labels {
			b.config.Config.Labels[k] = v
		}
		return nil
	case "EXPOSE":
		if b.config.Config.ExposedPorts == nil {
			b.config.Config.ExposedPorts = map[string]struct{}{}
		}
		for _, port := range strings.Fields(ins.Args) {
			b.config.Config.ExposedPorts[port] = struct{}{}
		}
		return nil
	default:
		return fmt.Errorf("unsupported instruction %s", ins.Cmd)
	}
}

// runStep executes a RUN command in a container built from the current
// workspace and snapshots the resulting filesystem changes.
func (b *builder) runStep(ctx context.Context, argv []string, workdir string) error {
	if len(argv) == 0 {
		return fmt.Errorf("RUN needs a command")
	}
	cfg := &runtime.Config{
		Image:    "minibox-build",
		Args:     argv,
		Env:      b.config.Config.Env,
		Cwd:      workdir,
		User:     b.config.Config.User,
		Hostname: "build",
		// The workspace overlay is owned by the builder, so the container must
		// not create one of its own.
		Rootfs:   runtime.Rootfs{Merged: b.merged, Overlay: false},
		Network:  runtime.Network{Mode: "none", Hostname: "build"},
		Security: runtime.Security{Seccomp: "unconfined", NoNewPrivs: false, Rootless: false},
		Created:  time.Now().UTC(),
	}
	// Build steps run with the full capability set: they are the equivalent of
	// running the Dockerfile's commands on the host, and the default
	// restricted set breaks common package managers.
	caps, err := resolveCaps([]string{"ALL"}, nil)
	if err != nil {
		return err
	}
	cfg.Security.Capabilities = caps

	c, err := container.Create(b.env.Store, cfg, "")
	if err != nil {
		return err
	}
	startOpts := container.StartOptions{
		Stdin:    b.env.Stdin,
		Stdout:   b.env.StdoutFile,
		Stderr:   b.env.StderrFile,
		Progress: b.env.Progress,
		NoCgroup: true,
	}
	started, err := container.Start(ctx, b.env.Store, c.ID, startOpts)
	if err != nil {
		cleanupFailedStart(b.env, c.ID)
		return fmt.Errorf("RUN %s: %w", strings.Join(argv, " "), err)
	}
	code, err := started.Wait(ctx)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("RUN %s exited with code %d", strings.Join(argv, " "), code)
	}
	return b.snapshot()
}

// copyStep copies files from the build context into the workspace.
func (b *builder) copyStep(ins build.Instruction) error {
	if from := ins.Flags["from"]; from != "" {
		return fmt.Errorf("COPY --from=%s is not supported yet: multi-stage copies need the stage rootfs, which this version does not mount", from)
	}
	fields := splitFields(ins.Args)
	if len(fields) < 2 {
		return fmt.Errorf("%s needs at least a source and a destination", ins.Cmd)
	}
	dest, err := build.ExpandArgs(fields[len(fields)-1], b.vars)
	if err != nil {
		return err
	}
	dest = strings.TrimSpace(dest)
	destIsDir := strings.HasSuffix(dest, "/") || dest == "." || len(fields) > 2
	destPath := filepath.Join(b.merged, filepath.Clean("/"+dest))
	for _, pattern := range fields[:len(fields)-1] {
		expanded, err := build.ExpandArgs(pattern, b.vars)
		if err != nil {
			return err
		}
		sources, err := build.ExpandCopySources(b.contextDir, expanded)
		if err != nil {
			return err
		}
		for _, src := range sources {
			info, err := os.Lstat(src)
			if err != nil {
				return err
			}
			target := destPath
			if info.IsDir() || destIsDir {
				target = filepath.Join(destPath, filepath.Base(src))
			}
			if err := copyInto(src, target); err != nil {
				return err
			}
		}
	}
	return b.snapshot()
}

// envStep applies ENV and updates the substitution variables.
func (b *builder) envStep(ins build.Instruction) error {
	kv := parseKeyValues(ins.Args)
	if len(kv) == 0 {
		return fmt.Errorf("ENV needs KEY=VALUE pairs")
	}
	for k, v := range kv {
		b.vars[k] = v
		replaced := false
		for i, existing := range b.config.Config.Env {
			if strings.HasPrefix(existing, k+"=") {
				b.config.Config.Env[i] = k + "=" + v
				replaced = true
				break
			}
		}
		if !replaced {
			b.config.Config.Env = append(b.config.Config.Env, k+"="+v)
		}
	}
	return nil
}

// snapshot tars the workspace's upper directory into a layer blob, replacing
// the previous cumulative delta.
func (b *builder) snapshot() error {
	digest, size, err := b.tarUpper()
	if err != nil {
		return err
	}
	b.delta = digest
	b.deltaSz = size
	return nil
}

// tarUpper writes a gzipped tar of the overlay upper directory.
func (b *builder) tarUpper() (string, int64, error) {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.Walk(b.upper, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if path == b.upper {
				return nil
			}
			rel, err := filepath.Rel(b.upper, path)
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if link, lerr := os.Readlink(path); lerr == nil {
					hdr.Linkname = link
				}
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				f.Close()
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(tw.Close())
	}()
	digest, size, err := b.env.Images.StoreBlobFromReader(pr)
	if err != nil {
		pr.CloseWithError(err)
		return "", 0, err
	}
	return digest, size, nil
}

// readCache loads a cache entry when its blob is still present.
func (b *builder) readCache(key string) (buildCacheEntry, bool) {
	var entry buildCacheEntry
	path := filepath.Join(b.env.Store.Root, "build", "cache", key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return entry, false
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return entry, false
	}
	if entry.DeltaDigest != "" && !b.env.Images.HasBlob(entry.DeltaDigest) {
		return entry, false
	}
	return entry, true
}

// writeCache stores a cache entry for an instruction prefix.
func (b *builder) writeCache(key string) {
	dir := filepath.Join(b.env.Store.Root, "build", "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	entry := buildCacheEntry{DeltaDigest: b.delta, DeltaSize: b.deltaSz, Config: b.config}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, key+".json"), data, 0o600)
}

// buildCommand implements `minibox build`.
func buildCommand(env *Env, args []string) error {
	fs := newFlagSet("build")
	tag := fs.String("t", "", "image tag")
	file := fs.String("f", "", "Dockerfile path")
	noCache := fs.Bool("no-cache", false, "ignore the layer cache")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rest := fs.Args()
	contextDir := "."
	if len(rest) > 0 {
		contextDir = rest[0]
	}
	if *noCache {
		if err := os.RemoveAll(filepath.Join(env.Store.Root, "build", "cache")); err != nil {
			return err
		}
	}
	imageTag := *tag
	if imageTag == "" {
		base := filepath.Base(mustAbs(contextDir))
		imageTag = sanitizeName(base) + ":latest"
	}
	_, err := buildImage(context.Background(), env, contextDir, *file, imageTag)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Successfully built %s\n", imageTag)
	return nil
}

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// imageChainID hashes layer digests into a chain id.
func imageChainID(layers []image.LayerRef) string {
	digests := make([]string, 0, len(layers))
	for _, l := range layers {
		digests = append(digests, l.Digest)
	}
	return storage.ChainID(digests)
}

// tagName returns the repository part of a tag.
func tagName(tag string) string {
	name := tag
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// tagPart returns the tag part of a tag.
func tagPart(tag string) string {
	if i := strings.IndexByte(tag, ':'); i >= 0 {
		return tag[i+1:]
	}
	return "latest"
}
