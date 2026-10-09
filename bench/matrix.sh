#!/usr/bin/env bash
# Pull-time, memory, disk and run-time matrix over bench/images.txt.
#
#   bench/matrix.sh                 # every image in bench/images.txt
#   bench/matrix.sh alpine:3.20     # just these images
#   BENCH_APPEND=1 bench/matrix.sh  # also append the table to docs/BENCHMARKS.md
#
# Each image is pulled into a fresh MINIBOX_ROOT unless MINIBOX_ROOT is set,
# measured with /usr/bin/time -v (wall clock and peak RSS), and its on-disk
# footprint is reported. When hyperfine and a container runtime (docker,
# podman) are available, the run comparison from bench/run.sh is appended too.
set -uo pipefail

cd "$(dirname "$0")/.."

BIN="${MINIBOX_BIN:-$PWD/bin/minibox}"
if [ ! -x "$BIN" ]; then
  echo "building minibox" >&2
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/minibox
fi

if [ "$#" -gt 0 ]; then
  IMAGES=("$@")
else
  IMAGES=()
  while read -r line; do
    [ -z "$line" ] && continue
    case "$line" in \#*) continue;; esac
    IMAGES+=("${line%%[[:space:]]*}")
  done < bench/images.txt
fi

time_v=/usr/bin/time
[ -x "$time_v" ] || time_v=""

report() { printf '%s\n' "$*" | tee -a "$OUT"; }

OUT="$(mktemp)"
report "| image | layers | pull wall | peak RSS | blobs on disk | layers on disk |"
report "|---|---|---|---|---|---|"

for image in "${IMAGES[@]}"; do
  root="$(mktemp -d /tmp/minibox-matrix-XXXXXX)"
  export MINIBOX_ROOT="$root"
  log="$root/pull.log"
  start=$(date +%s.%N)
  if [ -n "$time_v" ]; then
    # time -o keeps its report out of the command's own stderr, which carries
    # the progress lines this script counts.
    "$time_v" -v -o "$root/time.log" "$BIN" pull "$image" >"$log" 2>&1
  else
    "$BIN" pull "$image" >"$log" 2>&1
  fi
  rc=$?
  end=$(date +%s.%N)
  if [ $rc -ne 0 ]; then
    report "| $image | - | FAILED (rc=$rc) | - | - | - |"
    echo "--- $image failed ---" >&2
    tail -3 "$log" >&2
    rm -rf "$root"
    continue
  fi
  wall=$(awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f s", e - s }')
  layers=$(grep -c "unpacked layer" "$log" || true)
  rss="-"
  if [ -n "$time_v" ] && grep -q "Maximum resident set size" "$root/time.log"; then
    rss=$(awk '/Maximum resident set size/ { printf "%.1f MB", $6/1024 }' "$root/time.log")
  fi
  blobs=$(du -sh "$root/blobs" 2>/dev/null | cut -f1)
  layersdisk=$(du -sh "$root/layers" 2>/dev/null | cut -f1)
  [ -n "$blobs" ] || blobs="-"
  [ -n "$layersdisk" ] || layersdisk="-"
  report "| $image | $layers | $wall | $rss | ${blobs:--} | ${layersdisk:--} |"
  rm -rf "$root"
done

echo
echo "note: 'run' latency against docker/podman/crun is measured by bench/run.sh"
echo "      (needs a host where those runtimes can actually start containers)."

if [ "${BENCH_APPEND:-0}" = "1" ]; then
  {
    echo
    echo "### $(date -u +%Y-%m-%dT%H:%M:%SZ) - $(uname -r) - $(nproc) cpus"
    echo
    cat "$OUT"
  } >> docs/BENCHMARKS.md
  echo "appended to docs/BENCHMARKS.md"
fi
rm -f "$OUT"

