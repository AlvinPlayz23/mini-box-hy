# Security model

This document describes what minibox does, what it does not do, and how it can
hurt you. It deliberately makes no claim of parity with Docker's or Podman's
security record: minibox is a young runtime with far less adversarial testing.

## Threat model

minibox assumes:

* the host kernel is trusted and reasonably current;
* the images it runs are untrusted content, but the user chose them;
* the user running minibox is trusted with their own account (rootful minibox is
  equivalent to running the container's process as root on the host);
* the host filesystem outside `$MINIBOX_ROOT` must never be modified by
  container setup or by image extraction.

It does **not** defend against a hostile kernel, a hostile root user, side
channels (Spectre-class), or a compromised registry serving malicious *content*
(digests prove integrity, not intent).

## What is enforced by default

* **Namespaces:** new PID, mount, UTS, IPC and network namespaces per container;
  the mount namespace is made private so container mounts never propagate out.
* **`pivot_root`:** the old root is detached and removed, so the host filesystem
  is unreachable from inside.
* **Capabilities:** the Docker-like default set (CHOWN, DAC_OVERRIDE, FOWNER,
  FSETID, KILL, SETGID, SETUID, SETPCAP, NET_BIND_SERVICE, NET_RAW, SYS_CHROOT,
  MKNOD, AUDIT_WRITE, SETFCAP); `--cap-drop`/`--cap-add` adjust it and the
  bounding set is reduced, so capabilities cannot be regained through setuid
  binaries.
* **`no_new_privs`:** set before `execve`, so setuid binaries and file
  capabilities cannot escalate.
* **seccomp:** a default filter with `EPERM` as its default action, denying
  Docker's documented dangerous set (`mount`, `ptrace`, `bpf`, `kexec_load`,
  `init_module`, `unshare`, `setns`, `reboot`, `swapon`, `keyctl`,
  `perf_event_open`, `userfaultfd`, ...). `--seccomp unconfined` disables it.
* **Masked paths:** `/proc/kcore`, `/proc/keys`, `/proc/timer_list`,
  `/proc/sched_debug`, `/sys/firmware` and friends are replaced with an empty
  read-only tmpfs or `/dev/null`.
* **Read-only paths:** `/proc/sys`, `/proc/bus`, `/proc/fs`, `/proc/irq`,
  `/proc/sysrq-trigger` are remounted read-only.
* **`--read-only`:** remounts the container root filesystem read-only (mounts
  such as `/proc`, `/dev` and volumes stay writable).
* **`--user`:** runs the container command as the given uid/gid, resolved
  against the image's `/etc/passwd` and `/etc/group` when names are used.
* **Image extraction:** layer entries with absolute paths or `..` components are
  rejected, as are writes through symlinks that point outside the layer
  directory; extraction also aborts when a layer exceeds the configured
  unpacked-size limit.

## Known gaps

1. **The default seccomp profile is permissive.** It allows every syscall this
   kernel knows except the documented denied set, rather than a hand-curated
   minimal list. A stricter profile can be built by the same code path
   (`Profile.AllowEveryKnownSyscall = false` plus an `Allow` list) but is not
   exposed by the CLI yet.
2. **`--cap-add SYS_ADMIN` does not re-enable syscalls blocked by seccomp.**
   Docker's profile makes some rules capability-conditional; minibox does not.
3. **No LSM integration.** There is no AppArmor, SELinux or Landlock profile.
4. **Rootless `exec` is unsupported** and fails with an explicit message.
5. **Rootless mode is only as strong as user namespaces.** Without a populated
   subuid/subgid range, container root maps to the single calling user, so
   containers share that identity; with `newuidmap`, ranges come from the
   system's configuration.
6. **`--network host` removes network isolation entirely**, by design.
7. **Bridge mode requires root** and adds NAT rules; a container can reach the
   host's network through them. `--network none` is the isolated choice.
8. **No image signature or policy verification.** Digests are verified during
   pull (corruption is detected), but nothing checks who signed an image.
9. **No resource limits on a host without cgroup v2.** minibox refuses to start
   a container that asks for limits on such a host, but a container without
   limits can consume all host memory/CPU (that is the same behaviour as a
   normal process).
10. **`/dev` in unprivileged containers is borrowed from the host** (bind mounts
    of `/dev/null`, `/dev/urandom`, ...), so device nodes are host-owned.
11. **The built-in `--init` reaper does not reap double-forked orphans beyond its
    own children** and does not implement `SIGCHLD`-based zombie accounting for
    processes in nested PID namespaces.
12. **No audit trail.** minibox writes state files and logs but no security
    events.

## Reporting

Treat minibox as you would treat a `chroot`-and-namespaces script you wrote
yourself: useful for development, not a boundary for hostile multi-tenant
workloads. Run untrusted code in a VM, or at minimum with `--network none`,
`--read-only`, `--user`, `--cap-drop ALL` and memory/pids limits.
