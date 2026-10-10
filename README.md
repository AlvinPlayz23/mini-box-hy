# minibox

A fast, daemonless, Docker-like container runtime in Go. One static binary, no
daemon, OCI images pulled straight from Docker Hub or any registry v2 endpoint.

```console
$ minibox run --rm alpine echo hi
hi
```

## Status

Milestones M1-M10 of [PLAN.md](PLAN.md) are implemented: namespaces and
`pivot_root`, cgroup v2 limits, layered storage with whiteout handling, the full
lifecycle CLI with a detached shim, registry pulls with digest verification and
streaming unpack, networking (`none`, `host`, `bridge`, `pasta`), the security
defaults (capability sets, `no_new_privs`, seccomp, masked paths, rootless user
namespaces), named volumes, health checks, restart policies, `minibox build`,
a compose-like `up`/`down`, a Docker-compatible API on a socket, and the M10
performance pass. Lazy pulling (eStargz/SOCI) is deliberately not implemented;
see [docs/DECISIONS.md](docs/DECISIONS.md).

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
$ make build            # bin/minibox, static, ~8 MB
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

# volumes
minibox volume create appdata
minibox run --rm -v appdata:/var/lib/appdata alpine sh -c 'echo hi > /var/lib/appdata/x'
minibox volume ls / inspect appdata / rm appdata / prune

# health checks and restart policies (restart needs -d)
minibox run -d --health-cmd 'wget -q -O- http://localhost/ || exit 1' \
    --health-interval 5s --health-retries 3 -p 8080:80 nginx:alpine
minibox run -d --restart on-failure:5 alpine sh -c 'do-work || exit 1'

# build images (Dockerfile subset: FROM RUN COPY ENV WORKDIR CMD ENTRYPOINT
# USER LABEL EXPOSE ARG) with layer caching
minibox build -t myapp:dev .

# compose-like projects
minibox up -d -f compose.yaml        # start every service in depends_on order
minibox down --volumes               # stop and remove the project

# Docker-compatible API on a unix socket (ping, version, containers, images)
minibox serve --socket /run/minibox/minibox.sock &
curl --unix-socket /run/minibox/minibox.sock http://localhost/_ping

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
$ minibox doctor         # what this host supports (caps, cgroups, overlay, nft, pasta)
$ make bench-compare     # minibox vs docker vs podman on debian and ollama
$ bench/matrix.sh        # pull/memory/disk across bench/images.txt (incl. an AI image)
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
* `minibox build` caches at instruction granularity but rebuilds the tagged
  image as base layers plus one cumulative delta layer, so images built here are
  not byte-identical to Docker's per-instruction layers.
* `COPY --from=<stage>` (multi-stage copies) is not implemented yet, and build
  steps run with `--network none`.
* Compose support covers services, images, build, command, environment, ports,
  volumes, depends_on, restart, healthcheck, user, working_dir, hostname,
  cap_add/cap_drop, mem_limit, cpus, pids_limit, network_mode, labels, tty and
  init; `profiles`, `secrets`, `configs`, `deploy` and `extends` are ignored.
* The API socket is not socket-activated; run `minibox serve` under a systemd
  unit if you want it on demand.
* `exec` works for rootful containers; rootless `exec` would need a
  user-namespace join, which is not implemented (see `docs/SECURITY.md`).
* A non-root `--user` runs with no capabilities, like Docker; `--cap-add` is
  ignored for it (minibox warns). Capabilities the host does not provide are
  skipped with a warning, and `minibox doctor` shows which ones.
* `run -d -t` does not allocate a pseudo-terminal yet; detached containers log
  to `containers/<id>/log` and `minibox logs -f` follows it.
* Images are unpacked per image (flattened chain): two images sharing a base
  layer store it twice. Layer-level sharing is future work.
* The seccomp default profile starts permissive-but-documented: it permits the
  syscalls this kernel knows except Docker's default denied set. Tightening it
  remains future work (see docs/SECURITY.md).
* Detached containers get no stdin stream; `run -d -t` allocates no pty.

## Documentation

* [PLAN.md](PLAN.md) - goals, milestones, acceptance criteria.
* [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) - process model and subsystems.
* [docs/DECISIONS.md](docs/DECISIONS.md) - dependencies and design decisions.
* [docs/BENCHMARKS.md](docs/BENCHMARKS.md) - measured numbers and how to
  reproduce them.
* [docs/SECURITY.md](docs/SECURITY.md) - threat model and known gaps.
