//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// deviceNode describes a character device created in /dev.
type deviceNode struct {
	name  string
	mode  uint32
	major uint32
	minor uint32
}

var containerDevices = []deviceNode{
	{"null", 0o666, 1, 3},
	{"zero", 0o666, 1, 5},
	{"full", 0o666, 1, 7},
	{"random", 0o666, 1, 8},
	{"urandom", 0o666, 1, 9},
	{"tty", 0o666, 5, 0},
}

// makePrivate makes the mount namespace private so container mounts never
// propagate back to the host.
func makePrivate() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w (a mount namespace is required)", err)
	}
	return nil
}

// bindMount bind-mounts source onto target.
func bindMount(source, target string, recursive bool) error {
	flags := uintptr(unix.MS_BIND)
	if recursive {
		flags |= unix.MS_REC
	}
	if err := unix.Mount(source, target, "", flags, ""); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w", source, target, err)
	}
	return nil
}

// remountReadOnly turns an existing bind mount read-only.
func remountReadOnly(target string) error {
	if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}

// mountReadOnlyPath makes an existing path read-only inside the container.
func mountReadOnlyPath(root, path string) error {
	target := filepath.Join(root, path)
	if _, err := os.Stat(target); err != nil {
		return nil // Paths the image does not provide are skipped.
	}
	if err := bindMount(target, target, true); err != nil {
		return err
	}
	return remountReadOnly(target)
}

// maskPath hides a path from the container: directories get an empty read-only
// tmpfs, files are replaced by /dev/null.
func maskPath(root, path string) error {
	target := filepath.Join(root, path)
	info, err := os.Stat(target)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_RDONLY, "size=0k,mode=000"); err != nil {
			return fmt.Errorf("mask directory %s: %w", path, err)
		}
		return nil
	}
	devNull := filepath.Join(root, "dev/null")
	if _, err := os.Stat(devNull); err != nil {
		// Without /dev/null, fall back to an empty read-only file mount.
		if err := unix.Mount("tmpfs", filepath.Dir(target), "tmpfs", unix.MS_RDONLY, "size=0k,mode=000"); err != nil {
			return fmt.Errorf("mask %s: %w", path, err)
		}
		return nil
	}
	if err := bindMount(devNull, target, false); err != nil {
		return fmt.Errorf("mask %s: %w", path, err)
	}
	return remountReadOnly(target)
}

// setupProc mounts a fresh procfs at root/proc.
func setupProc(root string) error {
	target := filepath.Join(root, "proc")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV)
	if err := unix.Mount("proc", target, "proc", flags, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

// setupSys mounts a read-only sysfs at root/sys, falling back to a read-only
// bind mount of the host's /sys when the kernel refuses a fresh sysfs mount
// (for example in a user namespace).
func setupSys(root string) error {
	target := filepath.Join(root, "sys")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	flags := uintptr(unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV)
	if err := unix.Mount("sysfs", target, "sysfs", flags, ""); err == nil {
		return nil
	}
	if err := bindMount("/sys", target, true); err != nil {
		return err
	}
	return remountReadOnly(target)
}

// setupDev builds a minimal /dev tree at root/dev.
func setupDev(root string) error {
	target := filepath.Join(root, "dev")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_NOSUID, "mode=755,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	for _, d := range containerDevices {
		path := filepath.Join(target, d.name)
		dev := int(unix.Mkdev(d.major, d.minor))
		err := unix.Mknod(path, unix.S_IFCHR|d.mode, dev)
		if err == nil {
			continue
		}
		if err != unix.EPERM {
			return fmt.Errorf("create /dev/%s: %w", d.name, err)
		}
		// Unprivileged containers cannot create device nodes; borrow the
		// host's instead.
		if f, cerr := os.Create(path); cerr == nil {
			f.Close()
		}
		if err := bindMount("/dev/"+d.name, path, false); err != nil {
			return fmt.Errorf("bind /dev/%s from the host: %w", d.name, err)
		}
	}
	links := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
		"ptmx":   "pts/ptmx",
	}
	for name, linkTarget := range links {
		path := filepath.Join(target, name)
		if err := os.Symlink(linkTarget, path); err != nil && !os.IsExist(err) {
			return fmt.Errorf("create /dev/%s: %w", name, err)
		}
	}
	pts := filepath.Join(target, "pts")
	if err := os.MkdirAll(pts, 0o755); err != nil {
		return err
	}
	devptsOpts := "newinstance,ptmxmode=0666,mode=0620"
	if err := unix.Mount("devpts", pts, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, devptsOpts); err != nil {
		return fmt.Errorf("mount /dev/pts: %w", err)
	}
	shm := filepath.Join(target, "shm")
	if err := os.MkdirAll(shm, 0o1777); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", shm, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev/shm: %w", err)
	}
	return nil
}

// setupVolume prepares one configured mount inside the container root.
func setupVolume(root string, m Mount) error {
	target := filepath.Join(root, filepath.Clean("/"+m.Destination))
	switch m.Type {
	case "tmpfs":
		if err := os.MkdirAll(target, 0o1777); err != nil {
			return err
		}
		opts := "mode=1777"
		for _, o := range m.Options {
			opts += "," + o
		}
		if err := unix.Mount("tmpfs", target, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, opts); err != nil {
			return fmt.Errorf("mount tmpfs on %s: %w", m.Destination, err)
		}
		return nil
	case "bind", "":
		info, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("volume source %s: %w", m.Source, err)
		}
		if info.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if f, err := os.Create(target); err == nil {
				f.Close()
			}
		}
		if err := bindMount(m.Source, target, true); err != nil {
			return err
		}
		if m.ReadOnly {
			return remountReadOnly(target)
		}
		return nil
	default:
		return fmt.Errorf("unsupported mount type %q for %s", m.Type, m.Destination)
	}
}

// pivot switches the process into the prepared root.
func pivot(root string) error {
	putOld := filepath.Join(root, ".minibox-oldroot")
	if err := os.MkdirAll(putOld, 0o700); err != nil {
		return fmt.Errorf("create old-root directory: %w", err)
	}
	if err := unix.PivotRoot(root, putOld); err != nil {
		return fmt.Errorf("pivot_root(%s): %w", root, err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after pivot_root: %w", err)
	}
	if err := unix.Unmount("/.minibox-oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Remove("/.minibox-oldroot"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old root directory: %w", err)
	}
	return nil
}

// bringUpLoopback enables the loopback interface inside the container's network
// namespace. Failure is not fatal: a container without networking may still be
// useful.
func bringUpLoopback() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
