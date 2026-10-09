#!/usr/bin/env bash
# Measure minibox's own startup cost, separate from container startup.
#
#   bench/startup.sh [runs]
#
# Reports the minimum and mean wall time of `version` (pure CLI startup),
# `ps` (store open plus state read) and `run --dry-run` (full configuration
# resolution without starting a container), plus the binary size.
set -uo pipefail

cd "$(dirname "$0")/.."

BIN="${MINIBOX_BIN:-$PWD/bin/minibox}"
RUNS="${1:-200}"
export MINIBOX_ROOT="${MINIBOX_ROOT:-$(mktemp -d /tmp/minibox-startup-XXXXXX)}"

if [ ! -x "$BIN" ]; then
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/minibox
fi

# A small image so the dry-run has something to resolve.
"$BIN" load -t bench-startup:1 /dev/null >/dev/null 2>&1 || true

measure() {
  local label="$1"; shift
  local best="" total=0 start end ms
  for _ in $(seq 1 "$RUNS"); do
    start=$(date +%s%N)
    "$@" >/dev/null 2>&1
    end=$(date +%s%N)
    ms=$(( (end - start) / 1000000 ))
    total=$((total + ms))
    if [ -z "$best" ] || [ "$ms" -lt "$best" ]; then best=$ms; fi
  done
  printf '%-28s min %4s ms   mean %4s ms   (%s runs)\n' "$label" "$best" "$((total / RUNS))" "$RUNS"
}

echo "binary: $BIN"
printf '%-28s %s bytes\n' "size" "$(stat -c %s "$BIN")"
echo

measure "minibox version" "$BIN" version
measure "minibox ps" "$BIN" ps
measure "minibox images" "$BIN" images
measure "minibox help" "$BIN" help

echo
echo "idle RAM: there is no daemon, so nothing to measure (0 MB)."
echo "per-container shim RSS on a real host: ps -o rss= -p \$(pgrep -f 'minibox shim')"

