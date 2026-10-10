# Benchmark harness

`bench/run.sh` measures `minibox run --rm` against `docker`, `podman` and
`crun` on the same machine, using the same image and command.

```sh
make bench                       # alpine:3.20 true, 50 runs
RUNS=100 bench/run.sh debian:stable-slim cat /etc/os-release
BENCH_APPEND=1 bench/run.sh      # also append the numbers to docs/BENCHMARKS.md
```

`bench/compare.sh` is the three-runtime comparison: debian and ollama, measured
with minibox, docker and podman side by side. Per runtime and image it records
pull time and image size on disk, warm `run --rm IMAGE true` latency
(hyperfine), and for ollama the time from `run -d -p 11434:11434` until
`/api/version` answers plus the serving container's resident memory. It also
prints the idle memory of each runtime's background processes, where minibox's
zero-daemon design shows up directly.

```console
$ make bench-compare                    # debian + ollama, all three runtimes
$ bench/compare.sh debian:stable-slim   # one image
$ PULL_ONLY=1 bench/compare.sh          # measurement without containers
$ BENCH_APPEND=1 bench/compare.sh       # append both tables to docs/BENCHMARKS.md
```

Runtimes that are installed but unusable (no `docker info`, no `podman info`)
are reported by name in the report header with the reason, and a run where
fewer than two runtimes could be measured exits non-zero so CI notices. The
ollama serving test needs working published ports (`-p`), so run it rootful
(bridge) or with pasta.

`bench/matrix.sh` is the image spread: it pulls everything listed in
`bench/images.txt` (minimal, apt-based, multi-layer, language runtime, and a
multi-GiB AI image) and records pull wall time, peak RSS and disk per image.
`bench/startup.sh` measures the CLI's own startup floor.

Requirements:

* `hyperfine` for the primary path; without it the script falls back to a shell
  timing loop (less accurate, still useful).
* `docker`/`podman`/`crun` are optional: each is measured only when present.
* `crun` also needs a prepared bundle (`bench/make-bundle.sh`, run as root).

What is measured:

* wall-clock time per `run --rm` (cold and warm, as hyperfine reports it),
* peak RSS while pulling a ~100 MB image (streaming unpack check),
* binary size via `make size`.

Record every run in `docs/BENCHMARKS.md` together with the kernel version and
CPU count, because container startup cost is dominated by the host's kernel and
filesystem.

