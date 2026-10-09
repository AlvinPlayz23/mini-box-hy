# minibox: a fast, daemonless, Docker-like container runtime in Go

This file is the project plan. It is kept in the repository so that the working
agreements, milestones and acceptance criteria stay visible.

## 0. Working agreements

1. **Safety.** minibox calls `mount`, `unshare`, `pivot_root`, writes to
   `/sys/fs/cgroup` and creates network devices. Develop inside a **VM or
   disposable Linux box**. All state lives under a configurable root
   (`MINIBOX_ROOT`, default `/var/lib/minibox`; rootless default
   `~/.local/share/minibox`). Tests always set `MINIBOX_ROOT` to a temp dir.
2. **One milestone at a time**, committed as `M<n>: ...`.
3. **Measure, don't guess.** The benchmark harness is built before features and
   results are recorded in `docs/BENCHMARKS.md`.
4. **Keep dependencies minimal**; every dependency is justified in
   `docs/DECISIONS.md`.
5. **Keep it static and small**: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`.
6. **Linux only** (`//go:build linux`); pure logic stays testable without root.
7. **Ask before big forks**: record the choice in `docs/DECISIONS.md`.
8. **Update docs as you go.**

## 1. Goals and non-goals

Goals, in priority order: speed (warm `run --rm alpine true` under ~50 ms),
Docker-like UX, low RAM (no daemon), OCI compatibility, rootless support.

Non-goals for v1: macOS/Windows, Kubernetes/CRI, Swarm, full Docker API
compatibility, image building beyond a small Dockerfile subset.

## 2. Architecture

### 2.1 Process model (daemonless)

```
minibox run alpine sh
   |
   +- CLI resolves image from the local store (pulls if missing)
   +- Creates container dir, overlayfs mount, cgroup (v2)
   +- Re-execs itself: /proc/self/exe init   (new namespaces via SysProcAttr)
   |      parent <-> child synchronise over a socketpair
   +- init stage (inside the namespaces):
         pivot_root -> mount /proc /dev /sys -> set hostname
         -> drop capabilities -> no_new_privs -> seccomp -> execve(user command)
```

- **Foreground `run`:** the CLI process supervises the container (fastest path).
- **Detached `run -d`:** the CLI re-execs `minibox shim`, which holds the log
  file, waits for the container, writes the exit code to `state.json`, tears
  down cgroup/network and exits.
- **No daemon.** Idle RAM is zero.

### 2.2 Why the re-exec pattern

Go is multithreaded, so namespaces are entered by launching `/proc/self/exe` with
`os/exec` and `SysProcAttr`: `Cloneflags` (PID, mount, UTS, IPC, net, user),
`UseCgroupFD`/`CgroupFD` (`CLONE_INTO_CGROUP`), `UidMappings`/`GidMappings` and
`Pdeathsig`. The child blocks on a socketpair until the parent finishes the setup
that must happen from outside (cgroup limits, veth wiring), then execs.

### 2.3 Directory layout (runtime state)

```
$MINIBOX_ROOT/
  blobs/sha256/<hex>                   compressed layer blobs (kept until prune)
  layers/<chainid>/diff                unpacked, flattened layer chain
  images/<registry>/<repo>/<tag>.json  manifest + config + layer refs
  containers/<id>/
    config.json  state.json  lock
    upper/ work/ merged/               overlayfs dirs
    log                                stdout/stderr for detached containers
  networks/ipam/<id>.json              IP leases (file locked)
```

State mutations are guarded by `flock`; container ids are 64 hex characters and
any unique prefix is accepted (12 shown).

### 2.4 Source layout

```
cmd/minibox/main.go        subcommand dispatch (stdlib flag, no Cobra)
internal/cli/              command implementations + hidden helpers
internal/container/        lifecycle, state, shim glue, pty, prune
internal/runtime/          re-exec, init stage, namespaces, pivot_root, mounts
internal/cgroup/           cgroup v2 helpers
internal/image/            reference parsing, registry client, blob store, unpack
internal/storage/          overlayfs management, layer extraction
internal/network/          none/host/bridge/pasta, netlink, nftables, IPAM
internal/security/         capabilities, seccomp, default profiles
bench/                     benchmark scripts (hyperfine)
test/integration/          root-requiring tests, build tag `integration`
docs/                      ARCHITECTURE.md, DECISIONS.md, BENCHMARKS.md, SECURITY.md
```

### 2.5 Dependencies (allowed list)

`golang.org/x/sys/unix`, `github.com/vishvananda/netlink` (M7),
`github.com/vishvananda/netns`, `github.com/klauspost/compress` (zstd layers).
A pure-Go seccomp BPF builder is implemented in-tree (no CGO, no libseccomp, no
`x/net/bpf` dependency). The registry client is in-tree rather than
`go-containerregistry`. See `docs/DECISIONS.md` for the reasoning.

## 3. CLI surface

```
minibox run [flags] IMAGE [CMD...]
    -d --detach  --rm  --name NAME  -it  -e -v -w -u -p
    --network none|host|bridge|pasta   --hostname  --read-only
    --memory 64m  --memory-swap 128m  --cpus 0.5  --pids-limit 100
    --cap-add/--cap-drop  --seccomp default|unconfined  --init  --platform
minibox ps [-a] [--json]        minibox images [--json]
minibox pull IMAGE              minibox load [-t NAME:TAG] FILE
minibox exec [-it] ID CMD...    minibox logs [-f] ID
minibox stop [-t SECS] ID...    minibox rm [-f] ID...
minibox rmi IMAGE...            minibox inspect CONTAINER|IMAGE
minibox system prune            minibox version
minibox run-raw ROOTFS CMD...   (development command from M1)
```

Every listing command supports `--json`. Errors say what failed and what to do
next. `run` exits with the container's exit code.

## 4. Milestones

* **M1** isolated process + benchmark harness (`run-raw`, `bench/run.sh`).
* **M2** proper root filesystem (`pivot_root`, `/proc`, `/dev`, `/sys`, masked
  and read-only paths).
* **M3** cgroup v2 limits (`memory.max`, `cpu.max`, `pids.max`,
  `memory.swap.max`, `CLONE_INTO_CGROUP`, OOM reporting, cleanup).
* **M4** layered storage (blob store, flattened chain per image, whiteouts,
  extraction safety, overlayfs, `load`).
* **M5** lifecycle and CLI (`run`, `ps`, `stop`, `rm`, `logs`, `exec`,
  `inspect`, shim, signals, PTY, exit codes, log capture).
* **M6** image pull (reference parsing, registry v2, manifest lists, digest
  verification, parallel download, streaming unpack, gzip+zstd, GC).
* **M7** networking (`none`, `host`, `bridge` with veth + NAT + port publishing,
  `pasta` for rootless, IPAM, cleanup).
* **M8** security hardening and rootless (user namespaces, capability sets,
  `no_new_privs`, default seccomp profile, read-only rootfs, masked paths,
  `--user`, `docs/SECURITY.md`).
* **M9** optional extras (volumes, build, compose, healthchecks, Docker API,
  lazy pulling) - not started.
* **M10** performance pass (profiling, runtime tuning, lazy initialisation,
  final benchmark table) - not started.

## 5. Performance targets

| Metric | Target |
|---|---|
| Warm `run --rm alpine true` | under 50 ms |
| Warm `run --rm` with bridge networking | under 150 ms |
| Idle RAM | 0 (no daemon) |
| RSS of a detached-container shim | under 5 MB |
| Binary size | under 15 MB |
| Peak RSS pulling a 100 MB image | recorded, kept low via streaming |

## 6. Testing strategy

Unit tests (no root): reference parsing, digest verification, tar extraction
safety, whiteout handling, IPAM, config merge, state locking, seccomp filter
building, capability parsing, cgroup value parsing. Integration tests
(`//go:build integration`) cover namespace isolation, cgroup limits, overlay
behaviour, the pull-run cycle, networking and a final leak check; each uses a
temporary `MINIBOX_ROOT`. Fuzzing targets the tar extractor and the reference
parser. CI builds and runs the unit suite on every push.

## 7. Go-specific gotchas

`runtime.LockOSThread()` before per-thread syscall sequences; `setns` in the
re-exec'd child; CGO disabled; streaming tar extraction; close unneeded file
descriptors (`O_CLOEXEC`, `ExtraFiles` only for the sync socket); handle
`EINTR`/`EAGAIN` through `unix.*` wrappers.

## 8. Kernel and environment requirements

Linux 5.7+ (`CLONE_INTO_CGROUP`), 5.11+ for rootless overlayfs, cgroup v2
unified hierarchy. Bridge mode needs `CAP_NET_ADMIN` and nftables. Rootless
needs `newuidmap`/`newgidmap` (`uidmap`) with populated `/etc/subuid` and
`/etc/subgid`. Development extras: Go 1.23+, `hyperfine`, and for comparison
Docker, Podman and `crun`.

## 9. Disk and resource budget

| Item | Approx. size |
|---|---|
| Test images (alpine ~3.5 MB compressed, nginx ~25 MB, python-slim ~42 MB) | 100-200 MB |
| The minibox binary | ~7 MB (measured) |
| Image store | compressed blobs + unpacked chain per image |
| Per container | ~0 at start, grows with writes |

## 10. Definition of done for v1 (end of M8)

All M1-M8 acceptance criteria pass; unit and integration suites are green and
leak-free; `README.md` documents install, usage and limitations;
`docs/BENCHMARKS.md` shows real numbers; the images the user cares about run as
root and rootless; known gaps are listed honestly in `docs/SECURITY.md`.

## 11. Open questions

Answered in `docs/DECISIONS.md`.

## 12. References

OCI Image/Runtime/Distribution specs; `namespaces(7)`, `cgroups(7)`,
`pivot_root(2)`, `clone3(2)`, `mount_namespaces(7)`, `user_namespaces(7)`,
`capabilities(7)`, `seccomp(2)`, the kernel overlayfs documentation; runc,
crun, youki, Podman/conmon, containerd; Liz Rice's "Containers from Scratch".
