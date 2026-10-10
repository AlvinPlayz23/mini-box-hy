# Decisions

Every dependency and every design fork is recorded here, as PLAN.md section 0
requires. Format: what was decided, why, and what it costs.

## Dependencies

| Dependency | Why | Alternative rejected |
|---|---|---|
| `golang.org/x/sys/unix` | mount, pivot_root, prctl, capset, setns, ioctl, xattr, mknod wrappers | `syscall` alone lacks many of these wrappers and constants |
| `github.com/vishvananda/netlink` + `netns` | veth pairs, bridge, addresses, routes, and netlink handles bound to another namespace for bridge mode | hand-rolled netlink messages: hundreds of lines of message encoding that cannot be tested in a sandbox without `CAP_NET_ADMIN`, so correctness risk was judged too high for M7 |
| `github.com/klauspost/compress` (zstd) | OCI zstd layers (PLAN.md M6 requires gzip **and** zstd) | shelling out to `zstd`, or dropping zstd support |
| `gopkg.in/yaml.v3` | M9 compose support (`minibox up`/`down`): a correct YAML parser is not something to hand-roll, and this is the de-facto standard pure-Go one | writing a YAML subset parser (unsafe for real compose files) or requiring JSON compose files |

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

## M9 and M10 choices

**Named volumes are directories, not filesystem images.** `<root>/volumes/<name>/_data`
is bind-mounted into the container; there is no volume driver interface, no
NFS/CSI support and no quota. `docker volume`-style plugins are out of scope for
v1. In-use tracking is done by scanning container configs, which is O(containers)
and fine at this scale.

**Health probes reuse `exec-child`.** The supervisor cannot `setns` safely in a
multithreaded Go process, so each probe re-executes minibox and joins the
container's namespaces, exactly like `minibox exec`. Cost: one short-lived
process per probe. Benefit: one code path for "run a command inside a container",
and the probe genuinely runs inside the container's namespaces.

**Restart policies are enforced by the shim only.** A foreground `run` returns
when the container exits, so `--restart` without `-d` is a usage error rather
than a silently ignored flag. `unless-stopped` currently behaves like `always`
(minibox has no daemon remembering an explicit stop across a supervisor
restart); that is documented in the CLI help.

**Build caching is per instruction prefix, but the output is base layers plus
one cumulative delta.** Docker creates one layer per instruction; minibox tars
the whole overlay upper directory after each filesystem-changing step and keeps
only the last one as the image's layer. This keeps the code small and makes
whiteouts free (overlayfs writes them into the upper directory, and the
extractor already understands them), at the cost of not being able to share
intermediate layers between images. A future version can snapshot deltas by
diffing the upper directory between steps.

**Build steps run with the full capability set and no network.** Package
managers need capabilities that the Docker-like default set lacks, and the
build's commands are as trusted as the Dockerfile author. Networking is off by
default so builds are reproducible; a future `--network` build flag can relax
that.

**`COPY --from=<stage>` is not implemented.** Multi-stage builds parse, and the
target stage builds correctly, but copying from an earlier stage needs that
stage's rootfs to be built and kept, which is a larger change than the rest of
the build feature. The error message says exactly that.

**The Docker API is a documented subset, served from a normal process.** There
is no socket activation: the plan lists it as an option ("Decide only after M10
numbers"), and the M10 numbers show no warm-state problem to solve (there is no
daemon to warm up, and the CLI starts in milliseconds). A systemd unit with
`minibox serve` gives the same on-demand behaviour without a second code path.

**Lazy pulling (eStargz/SOCI) is not implemented.** The options were: (a) SOCI
or eStargz index plus HTTP range requests, which still needs a FUSE mount or
kernel overlay tricks to expose a partially downloaded filesystem, (b) a
custom lazy layer filesystem, (c) a "download the first layer and start"
heuristic, which is wrong for images whose entrypoint lives in the last layer.
All three are large, and (a) also needs an external index to exist for the
image. The simplest option that preserves correctness is the current streaming
unpack, which already never buffers a whole layer in memory; lazy pulling is
left as future work with this note as the starting point.

**Seccomp is installed immediately before `execve`, not during setup.** The
filter used to be installed before the supervisor handshake, which meant any
filter fault showed up as the process dying mid-setup ("container init exited
during setup: EOF") instead of as a legible error, and left setup code running
under the filter. It is now the last step before `execve` (both for the user
command and for the `--init` reaper), and the container init reports panics over
the sync socket so a crash can never again surface as a bare EOF.

**The seccomp filter is verified by evaluating it, not by inspecting it.**
`internal/security/seccomp_filter_test.go` contains a small BPF interpreter that
runs a generated program against seccomp_data exactly as the kernel does and
asserts the decision for allowed syscalls, denied syscalls, unknown syscalls, a
foreign architecture and the x32 ABI; a sweep asserts no native syscall can ever
reach the kill action. This was added after the filter turned out to have two
decision bugs that the earlier structural assertions could not see: an inverted
architecture check (`Jt`/`Jf` swapped, which killed every container on its first
syscall) and a wrong rule jump target (which made every allow-rule after the
first unreachable, so almost every syscall returned EPERM).
`seccomp_live_test.go` additionally installs the real filter in a child process
and requires it to survive, skipping on emulated kernels such as gVisor.

**Seccomp is still the permissive default.** M8 shipped the builder and the
Docker-like denied set; M9/M10 did not tighten it, because an incomplete
allow-list breaks real images and could not be integration-tested in the
development sandbox. `docs/SECURITY.md` lists it as gap #1 and the builder
already supports strict profiles.

## Capabilities on restricted hosts and with a non-root user

Two field failures shaped this area, both reported from a Railway VM running
`gccharles/nextjs-demo-app` (an image whose config sets `USER nextjs`):

**A non-root `--user` runs without capabilities.** `setuid` clears the permitted
and effective sets, and without CAP_SETPCAP in the effective set the kernel only
lets a process narrow its permitted set, so the capability installation that
followed the uid change failed with `capset: operation not permitted`. minibox
now skips capability installation when the resolved uid is non-zero, keeps
`no_new_privs` and the seccomp filter, and warns if `--cap-add` was requested
(which is ignored, exactly as Docker ignores it for non-root users). The
alternative, retaining capabilities across the uid change with
`PR_SET_KEEPCAPS`, would hand a non-root user capabilities that Docker does not,
so it was rejected. The same rule applies to `minibox exec --user`.

**Capabilities the host does not provide are dropped, not fatal.** Hosts
routinely ship a smaller bounding set than the Docker-like default (Railway,
Kubernetes restricted profiles, gVisor, any nested container). `ApplyCapabilities`
now intersects the request with the bounding set *and* with what the process can
actually install (`SplitGrantable`), applies the intersection, and returns the
names it could not grant so the container init prints one warning line and the
container still starts. `PR_CAPBSET_DROP` and the ambient clear tolerate `EPERM`
for the same reason: `--cap-drop ALL` removes CAP_SETPCAP itself, and a user
namespace does not need the bounding set reduced.

`minibox doctor` reports this class of problem directly: kernel and cgroup v2
state, the capability difference against the default set, seccomp and user
namespace availability, overlayfs, state-root writability, and whether `nft`,
`pasta`, `newuidmap` and `newgidmap` are present, each with the action to take.

## Deviations from PLAN.md worth knowing

* The plan's `docs/` list did not include `docs/SECURITY.md` in section 2.4 but
  M8 requires it; it exists.
* `internal/runtime` owns the container configuration type (the plan did not
  assign it), which keeps the dependency direction one-way:
  `cli -> container -> {runtime, storage, cgroup, image, network}`.
* `internal/container` reaches the network package through a small interface
  (`NetworkHook`) plus a registered teardown function, so it does not import
  `internal/network`.
