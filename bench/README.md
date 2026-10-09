# Benchmark harness

`bench/run.sh` measures `minibox run --rm` against `docker`, `podman` and
`crun` on the same machine, using the same image and command.

```sh
make bench                       # alpine:3.20 true, 50 runs
RUNS=100 bench/run.sh debian:stable-slim cat /etc/os-release
BENCH_APPEND=1 bench/run.sh      # also append the numbers to docs/BENCHMARKS.md
```

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

