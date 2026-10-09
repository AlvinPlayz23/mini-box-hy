# minibox

A fast, daemonless, Docker-like container runtime in Go. One static binary, no
daemon, OCI images pulled straight from Docker Hub or any registry v2 endpoint.

```console
$ minibox run --rm alpine echo hi
hi
```

## Status

Milestones M1-M8 of [PLAN.md](PLAN.md) are implemented: namespaces and
`pivot_root`, cgroup v2 limits, layered storage with whiteout handling, the full
lifecycle CLI with a detached shim, registry pulls with digest verification and
streaming unpack, networking (`none`, `host`, `bridge`, `pasta`) and the
security defaults (capability sets, `no_new_privs`, seccomp, masked paths,
rootless user namespaces). M9 (volumes beyond bind/tmpfs, build, compose) and
M10 (performance pass) are not started.

See [docs/BENCHMARKS.md](docs/BENCHMARKS.md) for what has been measured and
which parts still need a real Linux host to verify.

## Requirements

* Linux 5.7+ with the cgroup v2 unified hierarchy (5.11+ for rootless overlayfs).
* Root for bridge networking and the default `run` path; a normal user works
  through user namespaces (`newuidmap`/`newgidmap` from `uidmap` plus populated
  `/etc/subuid` and `/etc/subgid` are optional but recommended).
* `nft` (nftables) for NAT and port publishing, `pasta` (passt) for rootless
  networking. Neither is needed for `--network none` or `host`.

## Build and install

```console
$ make build            # bin/minibox, static, ~7 MB
$ sudo make install     # /usr/local/bin/minibox
```

All state lives under `$MINIBOX_ROOT` (default `/var/lib/minibox` for root,
`~/.local/share/minibox` otherwise), so a self-contained sandbox is one
environment variable away:

```console
$ MINIBOX_ROOT=$(mktemp -d) minibox run --rm alpine uname -a
```

## Usage

```console
# run, foreground or detached
minibox run --rm alpine echo hello
minibox run -d --name web -p 8080:80 nginx:alpine
minibox run -it --rm alpine sh
minibox run --rm --network none --memory 50m --cpus 0.5 --pids-limit 100 alpine sh
minibox run --rm -e FOO=bar -v $PWD:/work:ro -w /work alpine cat /work/README.md
minibox run --rm --user 1000:1000 --read-only --cap-drop ALL alpine id

# inspect and manage
minibox ps -a                 # add --json for machine-readable output
minibox logs -f web
minibox exec -it web sh
minibox stop -t 5 web
minibox rm -f web
minibox inspect web

# images
minibox pull alpine:3.20
minibox images
minibox load -t myapp:dev ./rootfs.tar.gz     # plain rootfs or docker save
minibox rmi myapp:dev
minibox system prune

# development helper from M1: no image store, just a rootfs directory
minibox run-raw ./rootfs /bin/sh -c 'hostname; echo $$'
```

`minibox run --dry-run ...` prints the fully resolved container configuration as
JSON without starting anything, which is handy for debugging flags.

## How it works

`minibox run` resolves the image in the local store (pulling it when missing),
creates the container directory and an overlayfs mount over the image's layer
chain, creates a cgroup v2 directory with the requested limits, then re-executes
itself as `minibox init` with new PID, mount, UTS, IPC, net (and optionally
user) namespaces. The init stage makes the mount namespace private, prepares
`/proc`, `/dev` and `/sys`, applies masked and read-only paths, `pivot_root`s
into the rootfs, sets the hostname, drops capabilities, sets `no_new_privs` and
installs the seccomp filter. The parent meanwhile puts the child in its cgroup
and wires networking; only then is the child released to `execve` the user
command. Detached containers are supervised by a tiny `minibox shim` process, so
there is no daemon and idle RAM is zero.

[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) has the details;
[docs/DECISIONS.md](docs/DECISIONS.md) records every dependency and design
choice.

## Testing

```console
$ make test              # unit tests, no root, no network (-short)
$ make test-network      # also runs live registry pull tests
$ make test-integration  # namespaces/cgroups/overlay/network, needs root
$ go test -run=XXX -fuzz=FuzzUnpackStaysInDest ./internal/storage/   # fuzzing
```

The integration suite encodes the acceptance criteria from PLAN.md and checks
for leaked mounts, cgroups, veths and nftables rules at the end. Run it on a
real Linux host or VM; `scripts/run-integration.sh` sets up the temp state root.

## Limitations (v1)

* Linux only, cgroup v2 only (no cgroup v1 or hybrid hosts).
* No image building, no Compose, no restart policies, no health checks yet.
* Volumes are bind mounts and tmpfs; there are no named volumes.
* `exec` works for rootful containers; rootless `exec` would need a
  user-namespace join, which is not implemented (see `docs/SECURITY.md`).
* `run -d -t` does not allocate a pseudo-terminal yet; detached containers log
  to `containers/<id>/log` and `minibox logs -f` follows it.
* Images are unpacked per image (flattened chain): two images sharing a base
  layer store it twice. Layer-level sharing is future work.
* The seccomp default profile starts permissive-but-documented: it permits the
  syscalls this kernel knows except Docker's default denied set. Tightening it
  is planned in M9/M10.

## Documentation

* [PLAN.md](PLAN.md) - goals, milestones, acceptance criteria.
* [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) - process model and subsystems.
* [docs/DECISIONS.md](docs/DECISIONS.md) - dependencies and design decisions.
* [docs/BENCHMARKS.md](docs/BENCHMARKS.md) - measured numbers and how to
  reproduce them.
* [docs/SECURITY.md](docs/SECURITY.md) - threat model and known gaps.
