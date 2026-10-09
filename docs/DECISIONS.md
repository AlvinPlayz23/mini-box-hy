# Decisions

Every dependency and every design fork is recorded here, as PLAN.md section 0
requires. Format: what was decided, why, and what it costs.

## Dependencies

| Dependency | Why | Alternative rejected |
|---|---|---|
| `golang.org/x/sys/unix` | mount, pivot_root, prctl, capset, setns, ioctl, xattr, mknod wrappers | `syscall` alone lacks many of these wrappers and constants |
| `github.com/vishvananda/netlink` + `netns` | veth pairs, bridge, addresses, routes, and netlink handles bound to another namespace for bridge mode | hand-rolled netlink messages: hundreds of lines of message encoding that cannot be tested in a sandbox without `CAP_NET_ADMIN`, so correctness risk was judged too high for M7 |
| `github.com/klauspost/compress` (zstd) | OCI zstd layers (PLAN.md M6 requires gzip **and** zstd) | shelling out to `zstd`, or dropping zstd support |

Deliberately **not** used:

* `github.com/opencontainers/runtime-spec/specs-go` - `internal/runtime/spec.go`
  defines only the fields minibox actually uses, which keeps the config small and
  stable.
* `github.com/google/go-containerregistry` - the in-tree registry client is about
  450 lines, covers anonymous and basic auth, manifest lists and digest
  verification, and avoids a large dependency tree and its startup cost.
* `golang.org/x/net/bpf` - seccomp filters are encoded by a 60-line assembler in
  `internal/security/seccomp.go`; `unix.SockFilter` is already available.
* `github.com/creack/pty` - `internal/container/pty.go` opens `/dev/ptmx`,
  unlocks it and reads the slave number with two ioctls.
* libseccomp/CGO - the binary must stay static.

## Storage

**Flattened chain per image** (`layers/<chainid>/diff`) instead of one directory
per layer stacked as multiple overlayfs lowers. Whiteouts are applied at unpack
time, where the semantics are unambiguous and testable, rather than relying on
the kernel's whiteout handling in lower layers. Cost: two images that share a
base layer store it twice. Benefit: the extraction safety properties (traversal,
symlink escape, opaque directories) are covered by unit tests and fuzzing, and
the overlay mount has a single lower directory, so the mount-option length limit
is not reachable. Per-layer directories are a possible M10 optimisation.

**Blobs are kept after unpacking** (PLAN.md open question 2, default answer).
`system prune` removes unreferenced blobs and chains.

**Device nodes are skipped when unprivileged.** A layer containing `/dev`
entries cannot create them without privileges, so `mknod` returning `EPERM` is
treated as "skip this entry" and the runtime supplies `/dev` itself.

## Runtime

**cgroup v2 only.** A cgroup v1 or hybrid host is rejected with a message
explaining `systemd.unified_cgroup_hierarchy=1`; supporting v1 would double the
cgroup code for hosts that are disappearing.

**`CLONE_INTO_CGROUP` with a fallback.** The child is started with `UseCgroupFD`
when the kernel supports it; the parent additionally writes the child's pid into
`cgroup.procs` (which is a no-op in the fast path) so kernels before 5.7 and
Go versions without clone3 support still work. This is also why the child blocks
on a socketpair: limits and networking are applied before the user command runs,
with no races.

**Mounts are prepared before `pivot_root`** with the root directory prefixed,
exactly as runc does, so the only post-pivot steps are hostname, loopback,
identity and seccomp. Masked and read-only paths therefore work without a second
mount namespace pass.

**`/sys` is a fresh read-only sysfs, with a read-only bind of the host `/sys` as
a fallback** for user namespaces where mounting sysfs is refused.

**Seccomp default profile is a permissive allow-list.** The BPF program has a
default action of `EPERM` and an allow entry for every syscall this
architecture's table knows, minus Docker's documented default denied set
(`mount`, `ptrace`, `bpf`, `kexec_load`, `unshare`, `setns`, `reboot`, ...). That
is functionally Docker's default profile, and the builder also supports strict
profiles (`AllowEveryKnownSyscall: false` plus an explicit `Allow` list), which
the CLI does not expose yet. Tightening the default list is M9/M10 work; the
reason it is not tightened now is that an incomplete allow-list breaks real
images and could not be integration-tested in the development sandbox.

**Rootless identity: single-ID mapping by default, subuid ranges when
`newuidmap`/`newgidmap` exist.** The kernel only allows a process to write its
own single-ID mapping, so multi-ID mappings require the setuid helpers; both
paths are implemented and chosen at start time.

**`--init` uses a bind-mounted copy of the binary** at `/dev/.minibox-init`
inside the rootfs, because after `pivot_root` the host filesystem (and therefore
`/proc/self/exe`) is gone.

**Rootless `exec` is not implemented.** Joining a user namespace requires a
single-threaded process, which a Go binary cannot guarantee; the command fails
with an explicit message rather than doing something unsafe.

**Log following polls** the log file every 100 ms instead of using inotify,
which keeps the dependency list at zero for M5 and works on every filesystem.

## Networking

**IPAM lives in `<root>/networks/ipam/<id>.json`** and is guarded by an `flock`
on `ipam.lock`; leases are reused when a container already has one and released
on teardown. A 10.88.0.0/16 subnet gives 65,534 addresses, far beyond the target
use case.

**Per-container nftables tables** (`minibox-<id12>`) hold the DNAT rules so that
teardown is a single exact `delete table`; the shared masquerade rule lives in
the `minibox` table and is removed by `system prune` when nothing is left.

**Bridge is the rootful default, `pasta` the rootless default** (PLAN.md open
question 1, default answer). `--network host` and `none` remain available and
are what the benchmark suite uses to isolate run latency from network setup.

**`pasta` is used as a process, not a library.** It is started with the
container's pid, stays in the foreground, and is killed on teardown.

## CLI and lifecycle

**stdlib `flag`, one `FlagSet` per command**, no Cobra: faster startup and no
dependency. Repeatable flags use a small `stringList` type.

**`run --dry-run`** prints the resolved configuration. It exists because the
resolution pipeline (image config merge, env, capabilities, mounts, network) is
otherwise only observable through a running container, and it makes that
pipeline testable on hosts without namespaces.

**Container ids are 64 hex characters**, and any unique prefix is accepted;
names are stored in `state.json` and resolved with the same prefix rules.

**`run-raw`** (M1) remains in the binary: it is the smallest reproduction of a
namespace bug and is what the M1 acceptance test uses.

## Open questions from PLAN.md section 11

1. **Primary use case:** long-running services with published ports. Answered by
   defaulting to `bridge` for root and `pasta`/`host` for rootless, and by
   implementing port publishing in bridge mode.
2. **Delete compressed blobs after unpacking:** no; keep them until
   `system prune` (the plan's default).
3. **Replace the registry client with a custom one after M6:** already custom;
   no dependency was added.
4. **Socket-activated helper for warm state:** not for now. The daemonless path
   is kept; this is revisited after M10 numbers exist.

## Deviations from PLAN.md worth knowing

* The plan's `docs/` list did not include `docs/SECURITY.md` in section 2.4 but
  M8 requires it; it exists.
* `internal/runtime` owns the container configuration type (the plan did not
  assign it), which keeps the dependency direction one-way:
  `cli -> container -> {runtime, storage, cgroup, image, network}`.
* `internal/container` reaches the network package through a small interface
  (`NetworkHook`) plus a registered teardown function, so it does not import
  `internal/network`.
