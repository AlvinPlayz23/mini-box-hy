# Architecture

minibox is a daemonless runtime: every command is a short-lived process that
does its work and exits, leaving only a container's own processes behind.

## Process model

```
minibox run --rm alpine sh
  |
  | 1. CLI process (supervisor)
  |    - resolve image in the store, pull when missing
  |    - create containers/<id>/ with config.json + state.json (flock)
  |    - mount overlayfs: lower=<chain dir> upper/work=<container dir>
  |    - create cgroup v2 dir, apply limits, open an O_PATH fd
  |
  | 2. re-exec /proc/self/exe init with CLONE_NEW{PID,NS,UTS,IPC,NET[,USER]}
  |    and CLONE_INTO_CGROUP (fallback: the parent writes cgroup.procs)
  |
  | 3. child (init)                     4. parent (supervisor)
  |    - make / private                   - wait for READY
  |    - bind rootfs, /proc, /dev, /sys   - move child into the cgroup (fallback)
  |    - masked + read-only paths         - apply limits
  |    - pivot_root, set hostname         - set up networking (veth, NAT, ports)
  |    - LockOSThread                     - send GO
  |    - uid/gid, capabilities,
  |      no_new_privs, seccomp
  |    - wait for GO
  |    - execve(user command)
```

The socketpair protocol is the synchronisation point that makes networking
deterministic: the user command cannot run before its veth, address, route, NAT
rules, `/etc/resolv.conf` and `/etc/hosts` are in place.

* **Foreground `run`:** the CLI is the supervisor; it waits for the container and
  returns the container's exit code.
* **Detached `run -d`:** the CLI forks `minibox shim`, which re-runs the same
  start path with stdout/stderr pointed at `containers/<id>/log`, reports
  `started <pid>` back over a pipe, then waits, records the exit status and
  tears everything down. The CLI exits immediately.
* **`exec`:** a re-executed `minibox exec-child` joins the container's mount,
  UTS, IPC, net and PID namespaces (on a locked thread) and `ForkExec`s the
  command, so the exec'd process lands in the container's PID namespace.
* **`--init`:** the running binary is bind-mounted at `/dev/.minibox-init`
  inside the rootfs; the init stage execs it as a tiny reaper that forwards
  signals and collects zombies.

## Storage

Images are content addressed. A layer blob is stored under
`blobs/sha256/<hex>` and verified against its digest while streaming to disk.
The layers of one image are unpacked, in order, into a single **flattened
chain** directory `layers/<chainid>/diff` (`chainid` is a hash over the ordered
layer digests). Applying a layer means extracting it over the existing tree,
which is where OCI whiteouts (`.wh.<name>`) and opaque markers
(`.wh..wh..opq`) are resolved by deleting the affected paths. Extraction
refuses absolute paths, `..` components and parents that are symlinks pointing
outside the destination.

Each container gets an overlayfs mount whose lower layer is that chain
directory and whose `upper`/`work` live in the container directory, so
containers share read-only layers and their writes are private.

## Networking

* `none` - a fresh network namespace with loopback.
* `host` - shares the host network namespace (no `CLONE_NEWNET`).
* `bridge` - creates `minibox0` (10.88.0.0/16), a veth pair per container, moves
  one end into the container namespace (renamed `eth0`), assigns an address from
  the file-locked IPAM, adds the default route, a host route back to the
  container, masquerading via nftables, and per-container DNAT rules in their own
  nftables table for `-p` publishing.
* `pasta` - rootless; `pasta --config-net` handles NAT, DNS and port forwarding
  for the container's namespace.

`/etc/resolv.conf` and `/etc/hosts` are written by the init stage from the
resolved configuration (so a read-only rootfs can be handled before it is
remounted).

## Volumes, health checks and restart policies

Named volumes live in `<root>/volumes/<name>/_data` with a `volume.json` beside
them; `-v NAME:DST` resolves a name to that directory (creating the volume on
first use) and host paths are used as-is. A volume referenced by any container
config cannot be removed without `-f`, and `system prune` collects the ones
nobody uses.

A health check runs `exec-child` (the same helper `minibox exec` uses) inside
the container on the configured interval and records `starting`, `healthy` or
`unhealthy` plus the consecutive failure count in `state.json`, where `ps` and
`inspect` read it. Failures inside `--health-start-period` are ignored.

Restart policies are enforced by the detached container's shim, which loops
start/wait/restart while the policy and the exit code say so, incrementing the
`restarts` counter in `state.json`. Foreground runs return when the container
exits and therefore refuse `--restart`.

## Build, compose and the API

`minibox build` parses the Dockerfile subset with `internal/build`, mounts one
overlay workspace over the base image's chain directory, runs each `RUN` as a
real container with that workspace as its root, copies `COPY` inputs through
the context-safety checks in `internal/build`, and after every filesystem
change tars the overlay's upper directory into a layer blob. Cache entries keyed
by the instruction prefix and the hashed contents of the `COPY` inputs let a
rebuild resume at the first changed instruction. The finished image is
registered exactly like a pulled one, so `run`, `ps` and `rmi` treat it the
same.

`minibox up` parses a compose file into a project, orders the services by
`depends_on`, maps each service onto a `runtime.Config` (the same mapping the
`run` flags use) and starts them detached with compose-style names
(`<project>_<service>_1`). `down` stops and removes every container of the
project and, with `--volumes`, the volumes it left behind.

`minibox serve` exposes the subset of the Docker Engine API implemented in
`internal/api` (ping, version, info, containers create/start/stop/kill/restart/
remove/wait/inspect/logs, images list/inspect/pull) over a unix socket, with
`internal/daemon` adapting the API's `Backend` interface onto the store and
lifecycle. The socket is not socket-activated; a systemd unit can start it on
demand.

## Security defaults

Capabilities are reduced to the Docker-like set (with `--cap-add`/`--cap-drop`),
the bounding set is dropped, `no_new_privs` is set, a default seccomp filter is
installed, `/proc/kcore` and friends are masked, `/proc/sys` and friends are
read-only, and the rootfs can be remounted read-only. Rootless mode creates a
user namespace, mapping container root to the caller (plus the subuid range when
`newuidmap` is available).

## State and locking

`containers/<id>/config.json` is the resolved, immutable description of a
container; `state.json` is the mutable status (running/exited, pid, exit code,
OOM flag, timestamps) and is written atomically via a temp file plus rename.
Every mutation takes an `flock` on `containers/<id>/lock`; store-wide operations
(create, prune) take `store.lock`. `system prune` removes dead containers,
unreferenced blobs and chains, leftover cgroups and overlay mounts.

## Error handling conventions

Errors say what failed and what to do next (for example the cgroup v1 message
explains `systemd.unified_cgroup_hierarchy=1`, and the overlayfs message explains
the capability requirement). Cleanup is registered as a stack of closures, so a
failure during setup unwinds exactly what was created, and `rm`, `stop` and
`prune` can always make progress on a container whose supervisor died.
