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
| `pull alpine:3.20` (3.5 MiB, 1 layer) | about 0.8 s wall |
| `pull nginx:alpine` (25.1 MiB, 8 layers) | 1.91 s wall, peak RSS 27.9 MB |
| `pull nginx:alpine` again (warm store) | 0.65 s wall |
| `pull python:3.12-slim` (42 MB compressed, 112 MB unpacked) | 2.98 s wall, peak RSS 27.2 MB |
| Disk after `pull nginx:alpine` | blobs 26 MB + layers 60 MB |
| Disk after `pull python:3.12-slim` | blobs 42 MB + layers 112 MB |

Peak RSS is flat at about 28 MB whether the image is 3.5 MiB or 42 MB, which is
the evidence that layers are streamed to disk and unpacked from disk instead of
being buffered in memory (PLAN.md section 5 asks for this number to be
recorded).

Test suite at the time of writing: 191 test/fuzz functions in 33 files (87 in
20 files at M8; M9 added volume, compose, build, API and daemon tests). The tar
extractor fuzzers ran about 200,000 executions (`FuzzCleanEntryPath`) and about
26,000 executions (`FuzzUnpackStaysInDest`) without finding a path that escapes
the layer directory.

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
