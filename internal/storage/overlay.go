//go:build linux

package storage

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// overlayMaxOptionLen keeps the mount option string below the kernel's 4096
// byte page limit.
const overlayMaxOptionLen = 3800

// RootfsOptions controls how a container root filesystem is prepared.
type RootfsOptions struct {
	// Overlay requests an overlayfs mount over the layer directories. When
	// false the single lower directory is used directly (read-only rootfs).
	Overlay bool
	// Rootless adds the userxattr option required by unprivileged overlayfs
	// (kernel 5.11+).
	Rootless bool
}

// RootfsPaths records the directories that make up a container root filesystem.
type RootfsPaths struct {
	Merged  string
	Upper   string
	Work    string
	Lower   []string
	Overlay bool
}

// PrepareRootfs creates the per-container overlay directories under
// containerDir and mounts them when requested.
func PrepareRootfs(containerDir string, lower []string, opts RootfsOptions) (RootfsPaths, error) {
	var p RootfsPaths
	if len(lower) == 0 {
		return p, fmt.Errorf("prepare rootfs: no layers given for %s", containerDir)
	}
	for _, l := range lower {
		if _, err := os.Stat(l); err != nil {
			return p, fmt.Errorf("prepare rootfs: layer %s is missing: %w", l, err)
		}
	}
	p = RootfsPaths{
		Merged:  filepath.Join(containerDir, "merged"),
		Upper:   filepath.Join(containerDir, "upper"),
		Work:    filepath.Join(containerDir, "work"),
		Lower:   lower,
		Overlay: opts.Overlay,
	}
	if !opts.Overlay {
		if len(lower) != 1 {
			return p, fmt.Errorf("prepare rootfs: overlayfs is disabled but %d layers were given", len(lower))
		}
		p.Merged = lower[0]
		return p, nil
	}
	for _, dir := range []string{p.Upper, p.Work, p.Merged} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return p, fmt.Errorf("prepare rootfs: create %s: %w", dir, err)
		}
	}
	if err := MountOverlay(lower, p.Upper, p.Work, p.Merged, opts.Rootless); err != nil {
		return p, err
	}
	return p, nil
}

// TeardownRootfs unmounts and removes the overlay directories of a container.
// It is best effort so that `rm` and `prune` can always make progress.
func TeardownRootfs(p RootfsPaths) error {
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if p.Overlay && p.Merged != "" && IsMounted(p.Merged) {
		record(UnmountOverlay(p.Merged))
	}
	if p.Overlay {
		for _, dir := range []string{p.Merged, p.Work, p.Upper} {
			if dir == "" {
				continue
			}
			record(os.RemoveAll(dir))
		}
	}
	return firstErr
}

// MountOverlay mounts an overlayfs with the given layers. lower[0] is the
// top-most lower layer, matching overlayfs ordering.
func MountOverlay(lower []string, upper, work, merged string, rootless bool) error {
	if len(lower) == 0 {
		return fmt.Errorf("mount overlay %s: no lower layers", merged)
	}
	if err := os.MkdirAll(merged, 0o755); err != nil {
		return fmt.Errorf("mount overlay %s: %w", merged, err)
	}
	options, fds, err := buildOverlayOptions(lower, upper, work, rootless)
	if err != nil {
		return err
	}
	defer func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Mount("overlay", merged, "overlay", 0, options); err != nil {
		return fmt.Errorf("mount overlay on %s: %w\n"+
			"hint: overlayfs requires CAP_SYS_ADMIN; run as root, or use a kernel with unprivileged "+
			"overlayfs support (5.11+) for rootless containers", merged, err)
	}
	return nil
}

// UnmountOverlay detaches an overlayfs mount, retrying lazily when the kernel
// reports that the mount is still busy.
func UnmountOverlay(merged string) error {
	if err := unix.Unmount(merged, 0); err == nil || err == unix.EINVAL || err == unix.ENOENT {
		return nil
	}
	if err := unix.Unmount(merged, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
		return fmt.Errorf("unmount %s: %w", merged, err)
	}
	return nil
}

// buildOverlayOptions renders the overlay mount option string, falling back to
// /proc/self/fd paths when the plain paths would exceed the kernel's limit. It
// returns the file descriptors that must stay open until the mount completes.
func buildOverlayOptions(lower []string, upper, work string, rootless bool) (string, []int, error) {
	render := func(paths []string) string {
		opts := "lowerdir=" + strings.Join(paths, ":") + ",upperdir=" + upper + ",workdir=" + work
		if rootless {
			opts += ",userxattr"
		}
		return opts
	}
	if opts := render(lower); len(opts) <= overlayMaxOptionLen {
		return opts, nil, nil
	}
	// Long layer chains overflow the option page: refer to the directories
	// through /proc/self/fd, which shortens each path to a few bytes.
	short := make([]string, 0, len(lower))
	var fds []int
	closeAll := func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}
	for _, dir := range lower {
		fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			closeAll()
			return "", nil, fmt.Errorf("open layer %s for overlay mount: %w", dir, err)
		}
		fds = append(fds, fd)
		short = append(short, "/proc/self/fd/"+strconv.Itoa(fd))
	}
	opts := render(short)
	if len(opts) > overlayMaxOptionLen {
		closeAll()
		return "", nil, fmt.Errorf("overlay option string is still %d bytes after shortening: too many layers", len(opts))
	}
	return opts, fds, nil
}

// MountInfo is one record from /proc/self/mountinfo.
type MountInfo struct {
	MountID    int
	ParentID   int
	MajorMinor string
	Root       string
	MountPoint string
	Options    string
	FSType     string
	Source     string
}

// ParseMountInfo reads the contents of /proc/self/mountinfo.
func ParseMountInfo(r io.Reader) ([]MountInfo, error) {
	var out []MountInfo
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+2 >= len(fields) {
			continue
		}
		id, err1 := strconv.Atoi(fields[0])
		parent, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, MountInfo{
			MountID:    id,
			ParentID:   parent,
			MajorMinor: fields[2],
			Root:       fields[3],
			MountPoint: unescapeMountField(fields[4]),
			Options:    fields[5],
			FSType:     fields[sep+1],
			Source:     unescapeMountField(fields[sep+2]),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unescapeMountField decodes the octal escapes mountinfo uses for spaces and
// other special characters.
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MountedPoints returns mount points under prefix, which is how minibox checks
// for leaked mounts.
func MountedPoints(mounts []MountInfo, prefix string) []string {
	var out []string
	for _, m := range mounts {
		if m.MountPoint == prefix || strings.HasPrefix(m.MountPoint, strings.TrimSuffix(prefix, "/")+"/") {
			out = append(out, m.MountPoint)
		}
	}
	return out
}

// IsMounted reports whether path is currently a mount point.
func IsMounted(path string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	mounts, err := ParseMountInfo(f)
	if err != nil {
		return false
	}
	for _, m := range mounts {
		if m.MountPoint == path {
			return true
		}
	}
	return false
}
