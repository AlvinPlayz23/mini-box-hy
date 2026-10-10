# Benchmarks

Two kinds of numbers live here:

1. **Verified measurements** taken in the development sandbox (a gVisor-based
   container). These cover everything that does not need namespaces: pulls,
   digest verification, unpacking, memory, disk and binary size.
2. **Pending measurements** for `run` latency, which need a real Linux host or
   VM because the sandbox cannot call `mount`, `unshare` or `pivot_root`.

## Verified numbers

Environment: Linux 4.19.0-gvisor, 2 vCPU, x86_64, warm page cache, Go 1.23.12,
`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`.

| Measurement | Value |
|---|---|
| Binary size | 7,131,288 bytes (6.8 MiB) after M8, 8.1 MB after M9/M10 - target under 15 MB |
| Pulls, memory and disk across six images | see the image matrix below |

Peak RSS stays between 27 MB and 52 MB whether the image is 2 MB or 3.5 GiB,
which is the evidence that layers are streamed to disk and unpacked from disk
instead of being buffered in memory (PLAN.md section 5 asks for this number to
be recorded).

Test suite at the time of writing: 191 test/fuzz functions in 33 files (87 in
20 files at M8; M9 added volume, compose, build, API and daemon tests). The tar
extractor fuzzers ran about 200,000 executions (`FuzzCleanEntryPath`) and about
26,000 executions (`FuzzUnpackStaysInDest`) without finding a path that escapes
the layer directory.

## Comparing against Docker, Podman and crun

Two harnesses cover this. `bench/run.sh` is the hyperfine latency comparison on
one image (`minibox` vs `docker` vs `podman` vs a raw `crun` bundle, 50 runs);
`bench/compare.sh` (next section) is the wider comparison over debian and ollama
that also measures pull time, disk, time-to-serving and memory. `bench/README.md`
documents the flags, and `bench/make-bundle.sh` builds the crun bundle.

**Status: no comparison numbers yet, and this is a limitation of the development
sandbox, not an omission.** The sandbox is a gVisor-based container:

* it cannot create namespaces (`unshare --mount --pid --fork` returns
  `Operation not permitted`), so neither minibox, docker, podman nor crun can
  start a container here;
* podman is not even installable (`apt-get install -y podman` -> `E: Unable to
  locate package podman`), and docker.io would need a working dockerd plus
  cgroups and iptables that this kernel surface does not provide.

So the honest state is: the method is fixed and reproducible, the numbers are
pending. On a real Linux host (or VM) with cgroup v2:

```console
$ make bench                                  # alpine:3.20 true, 50 runs
$ RUNS=100 bench/run.sh debian:stable-slim cat /etc/os-release
$ BENCH_APPEND=1 bench/run.sh                 # append the table here
```

The integration suite's `TestM10StartupAndSize` records the CLI-side floor
(process spawn plus runtime init) on any host that can run the binary, and
`bench/startup.sh` records the same numbers with a shell harness.

## Three-runtime comparison: minibox vs docker vs podman

`bench/compare.sh` (also `make bench-compare`) measures the same images with the
three runtimes, so the numbers are directly comparable:

| metric | how it is taken |
|---|---|
| pull | wall time of `pull IMAGE` per runtime |
| warm run | `run --rm --network host IMAGE true`, hyperfine `--warmup 2 --runs 10`, else a timing loop |
| on disk | minibox: blobs plus unpacked layers in the state root; docker/podman: `.Size` from `image inspect` |
| time to serving | ollama only: seconds from `run -d -p 11434:11434 ollama/ollama` until `GET /api/version` answers |
| container RSS | ollama only: resident memory of the serving container's host pid |
| background memory | sum of RSS of `dockerd`/`containerd`/`podman` processes, versus minibox's zero |

Default images: `debian:stable-slim` and `ollama/ollama:latest`. Knobs:
`RUNS`, `WARMUP`, `OLLAMA_PORT`, `READY_TIMEOUT`, `RUNTIMES`, `SKIP_PULL`,
`PULL_ONLY=1` (measurement without starting containers) and `BENCH_APPEND=1`
(append both markdown tables to this file). Runtimes that cannot be used are
named in the report header with the reason, and a run with fewer than two
usable runtimes exits non-zero.

**Status in the development sandbox (verified):** the harness runs end to end -
it measured minibox pulling debian (1.65 s, 29 MB of blobs and 76 MB of layers)
and ollama (72 s for 3.5 GiB), reported the exact reason `run` cannot start
containers here (the overlay mount is refused), and produced the ollama serving
table. Both table paths - warm-run latency and time-to-serving/RSS - were also
exercised against a stub runtime so the comparison columns are known to work.

**Real docker and podman numbers are still pending**, for the reasons in the
section above: this sandbox cannot create namespaces (so nothing can start a
container) and podman is not installable from its package index. Run
`make bench-compare` on a host with a working docker daemon and/or podman to fill
the table; the harness records everything needed, including why a runtime was
skipped.

## Image matrix (pull, memory, disk)

`bench/matrix.sh` pulls every image in `bench/images.txt` into a fresh state
root and records wall time, peak RSS and on-disk footprint:

```console
$ bench/matrix.sh                    # all images
$ bench/matrix.sh alpine:3.20        # just one
$ BENCH_APPEND=1 bench/matrix.sh     # append the table to this file
```

Measured in the development sandbox (gVisor, 2 vCPU, warm page cache). The
table below is exactly what `bench/matrix.sh` prints:

| image | layers | pull wall | peak RSS | blobs on disk | layers on disk |
|---|---|---|---|---|---|
| alpine:3.20 | 1 | 1.14 s | 29.0 MB | 3.5 MB | 7.5 MB |
| busybox:1.36 | 1 | 1.08 s | 27.3 MB | 2.2 MB | 4.3 MB |
| debian:stable-slim | 1 | 4.69 s | 29.5 MB | 29 MB | 76 MB |
| nginx:alpine | 8 | 8.77 s | 34.5 MB | 26 MB | 60 MB |
| python:3.12-slim | 4 | 10.06 s | 43.2 MB | 42 MB | 112 MB |
| ollama/ollama:latest (AI) | 4 | 4 m 29 s | 52.4 MB | 3.6 GB | 5.2 GB |

Wall times vary with registry rate limiting between runs (the earlier
single-image runs in this file were faster; Docker Hub throttles anonymous
pulls), so compare rows within one matrix run rather than across runs.

The AI image is the interesting one for memory: **3.5 GiB compressed and 5.2 GiB
unpacked, and peak RSS stayed at 52 MB**, which is the streaming-unpack claim
from M6 holding up at a scale 40x larger than the previous largest test image.
Its image config resolves correctly too (`Entrypoint ["/bin/ollama"]`,
`Cmd ["serve"]`, `ExposedPorts 11434/tcp`), so `minibox run -p 11434:11434
ollama/ollama` is the right command once a host that can run containers is
available. Note the disk numbers: this one image costs about 8.7 GB in the
store (blobs plus unpacked chain), which is why `system prune` and `rmi` exist.

`internal/image` carries the same matrix as a test
(`TestPullImageMatrix`: busybox, debian, python; set `MINIBOX_TEST_AI_IMAGES=1`
to add ollama), so the spread of image shapes is exercised by CI rather than
only by hand.

## M10 performance pass

The M10 work (profiling hooks, lazy initialisation, runtime tuning) produced
these verified numbers in the development sandbox:

| Measurement | Value |
|---|---|
| Binary size after M9/M10 | 8.1 MB (budget 15 MB; M9 added the compose parser, the Dockerfile parser, the API server and the daemon backend) |
| `minibox version` wall time | 16 ms minimum over 30 runs (bash spawn + exec + Go runtime init; the floor is the sandbox's process-spawn cost) |
| `minibox ps`, `minibox images` | 16-17 ms minimum, the same floor |
| Idle RAM | 0: no daemon process exists |

What changed in M10:

* `minibox version`, `help`, `init` and `minibox-init` no longer open the state
  root, create directories or touch the image store, so the most common
  "is it installed" commands do the minimum possible work.
* `MINIBOX_PROFILE=cpu|heap|trace|mutex|block|goroutine` makes any command
  write a profile (`MINIBOX_PROFILE_PATH` picks the file), which is how the hot
  path was inspected without adding flags to every subcommand.
* The detached shim calls `TuneShimRuntime`: `GOGC=50` and a 64 MiB soft memory
  limit (`MINIBOX_SHIM_MEMLIMIT` overrides it), which keeps a mostly-idle
  supervisor small.
* `--network none` containers skip reading `/etc/resolv.conf` entirely.

The remaining M10 items need a real host: `strace -T -f`/`perf trace` on the
run path (neither tool exists in the sandbox), the per-run syscall audit, and
the final comparison table against Docker, Podman and `crun`. `bench/startup.sh`
records the CLI-side numbers; `bench/run.sh` records the container-side ones.

## Pending measurements (need a real Linux host)

| Metric | Target | Command |
|---|---|---|
| Warm `run --rm alpine true` | under 50 ms | `make bench` |
| Warm `run --rm` with bridge networking | under 150 ms | `BENCH_IMAGE=... make bench` |
| Idle RAM | 0 (no daemon) | `ps` - there is no process to measure |
| RSS of a detached-container shim | under 5 MB | `ps -o rss= -p $(pgrep -f 'minibox shim')` |

`bench/run.sh` measures minibox against Docker, Podman and `crun` on the same
machine, the same image and the same command, using `hyperfine` when available
and a shell timing loop otherwise. It also prints the peak RSS of a pull with
`/usr/bin/time -v`. `BENCH_APPEND=1` appends the run to this file.

```console
$ make bench
$ RUNS=100 bench/run.sh debian:stable-slim cat /etc/os-release
$ BENCH_APPEND=1 bench/run.sh
```

Record the kernel version, CPU count and filesystem with every run: container
startup cost is dominated by those, not by the runtime.

## Notes for whoever runs the pending benchmarks

* Measure both cold and warm page cache; hyperfine's `--warmup` handles the
  second case.
* `--network host` isolates the run path from bridge setup; bridge setup time
  (veth creation, address, route, nftables) is usually the largest chunk of a
  networked `run`, so measure the two separately.
* `minibox run --dry-run` prints the resolved configuration without starting
  anything, which is the fastest way to check that a flag change did what you
  expected before benchmarking it.
