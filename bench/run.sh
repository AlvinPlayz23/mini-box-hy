#!/usr/bin/env bash
# Benchmark minibox against docker, podman and crun on the same command.
#
# Usage: bench/run.sh [image] [command...]
#   bench/run.sh alpine:3.20 true
#
# Results are printed and appended to docs/BENCHMARKS.md when BENCH_APPEND=1.
set -uo pipefail

cd "$(dirname "$0")/.."

IMAGE="${1:-alpine:3.20}"
shift || true
CMD=("$@")
if [ ${#CMD[@]} -eq 0 ]; then
  CMD=(true)
fi

BIN="${MINIBOX_BIN:-$PWD/bin/minibox}"
RUNS="${RUNS:-50}"
WARMUP="${WARMUP:-5}"
export MINIBOX_ROOT="${MINIBOX_ROOT:-$(mktemp -d /tmp/minibox-bench-XXXXXX)}"

if [ ! -x "$BIN" ]; then
  echo "building minibox" >&2
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/minibox
fi

# Make sure the image is local so we measure `run`, not `pull`.
"$BIN" pull "$IMAGE" >/dev/null 2>&1 || true

declare -a NAMES=() CMDS=()
add() { NAMES+=("$1"); CMDS+=("$2"); }

add minibox "$BIN run --rm --network host $IMAGE ${CMD[*]}"
docker_ok=0; podman_ok=0
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  add docker "docker run --rm --network host $IMAGE ${CMD[*]}"
  docker_ok=1
fi
if command -v podman >/dev/null 2>&1; then
  add podman "podman run --rm --network host $IMAGE ${CMD[*]}"
  podman_ok=1
fi
if command -v crun >/dev/null 2>&1 && [ -d /run/minibox-bench ]; then
  # crun needs a prepared bundle; bench/make-bundle.sh creates one.
  add crun "crun --root $MINIBOX_ROOT/crun run bench-bundle"
fi

echo "image:   $IMAGE ${CMD[*]}"
echo "runs:    $RUNS (warmup $WARMUP)"
echo "root:    $MINIBOX_ROOT"
echo

if command -v hyperfine >/dev/null 2>&1; then
  args=()
  for i in "${!NAMES[@]}"; do
    args+=(-n "${NAMES[$i]}" "${CMDS[$i]}")
  done
  hyperfine --warmup "$WARMUP" --runs "$RUNS" --export-json bench/results/last.json "${args[@]}" \
    2>&1 | tee bench/results/last.txt
else
  echo "hyperfine not found: falling back to a simple timing loop" >&2
  mkdir -p bench/results
  : > bench/results/last.txt
  for i in "${!NAMES[@]}"; do
    name="${NAMES[$i]}"; cmd="${CMDS[$i]}"
    start=$(date +%s.%N)
    for _ in $(seq 1 "$RUNS"); do bash -c "$cmd" >/dev/null 2>&1; done
    end=$(date +%s.%N)
    total=$(echo "$end - $start" | bc)
    avg=$(echo "scale=1; $total * 1000 / $RUNS" | bc)
    printf '%-10s %8s ms per run (%s runs)\n' "$name" "$avg" "$RUNS" | tee -a bench/results/last.txt
  done
fi

echo
echo "peak RSS of minibox pull (streaming unpack check):"
/usr/bin/time -v "$BIN" pull "$IMAGE" 2>&1 | grep -E "Maximum resident set size" || true

if [ "${BENCH_APPEND:-0}" = "1" ]; then
  {
    echo
    echo "### $(date -u +%Y-%m-%dT%H:%M:%SZ) - $(uname -r) - $(nproc) cpus"
    echo
    echo '```'
    cat bench/results/last.txt
    echo '```'
  } >> docs/BENCHMARKS.md
  echo "appended to docs/BENCHMARKS.md"
fi

