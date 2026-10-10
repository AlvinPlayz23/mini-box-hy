#!/usr/bin/env bash
# Compare minibox, docker and podman on the same images and the same workloads.
#
#   bench/compare.sh                       # debian + ollama, all three runtimes
#   bench/compare.sh debian:stable-slim    # just one image
#   PULL_ONLY=1 bench/compare.sh           # skip everything that starts containers
#   BENCH_APPEND=1 bench/compare.sh        # append the tables to docs/BENCHMARKS.md
#
# What is measured, per runtime and image:
#   pull        wall time to pull the image (cached pulls are near zero)
#   warm run    `run --rm IMAGE true`, hyperfine when available, else a timing loop
#   serving     ollama only: seconds from `run -d` until /api/version answers
#   RSS         ollama only: resident memory of the serving container process
#   on disk     the image's size as the runtime reports it (minibox: blobs+layers)
#
# Environment knobs:
#   RUNS=10              iterations for the warm-run measurement
#   WARMUP=2             hyperfine warmup runs
#   OLLAMA_PORT=11434    host port used for the ollama serving test
#   READY_TIMEOUT=180    seconds to wait for ollama to answer
#   RUNTIMES="minibox docker podman"   restrict the comparison
#   MINIBOX_ROOT=...     reuse a state root instead of a temp dir per image
#   SKIP_PULL=1          do not re-pull (useful when the store is warm)
set -uo pipefail

cd "$(dirname "$0")/.."

RUNS="${RUNS:-10}"
WARMUP="${WARMUP:-2}"
PORT="${OLLAMA_PORT:-11434}"
READY_TIMEOUT="${READY_TIMEOUT:-180}"
PULL_ONLY="${PULL_ONLY:-0}"
SKIP_PULL="${SKIP_PULL:-0}"
WANT_RUNTIMES="${RUNTIMES:-minibox docker podman}"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

if [ "$#" -gt 0 ]; then
  IMAGES=("$@")
else
  IMAGES=(debian:stable-slim ollama/ollama:latest)
fi

BIN="${MINIBOX_BIN:-$PWD/bin/minibox}"
if [ ! -x "$BIN" ]; then
  echo "building minibox" >&2
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/minibox || exit 1
fi

mkdir -p bench/results
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
REPORT="bench/results/compare-$STAMP.md"
: > "$REPORT"
say() { printf '%s\n' "$*" | tee -a "$REPORT"; }

# ---------------------------------------------------------------- runtimes ---
AVAILABLE=()
MISSING=()
want() { case " $WANT_RUNTIMES " in *" $1 "*) return 0;; *) return 1;; esac; }

if want minibox; then AVAILABLE+=(minibox); fi

if want docker; then
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    AVAILABLE+=(docker)
  else
    MISSING+=("docker (install docker-ce and make sure the daemon runs: docker info)")
  fi
fi

if want podman; then
  if command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
    AVAILABLE+=(podman)
  else
    MISSING+=("podman (install podman; rootless podman needs a user namespace, rootful needs a running service)")
  fi
fi

say "# minibox vs docker vs podman - $(date -u +%Y-%m-%dT%H:%M:%SZ)"
say ""
say "- host: $(uname -sr), $(nproc) cpus"
say "- runtimes measured: ${AVAILABLE[*]:-none}"
if [ "${#MISSING[@]}" -gt 0 ]; then
  for m in "${MISSING[@]}"; do say "- not measured: $m"; done
fi
say ""

# ------------------------------------------------------------- measuring ----
now() { date +%s.%N; }

# timed CMD...: prints seconds, or "failed" when the command fails.
timed() {
  local start end rc
  start="$(now)"
  "$@" >/dev/null 2>&1
  rc=$?
  end="$(now)"
  if [ "$rc" -ne 0 ]; then echo "failed"; return 1; fi
  awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }'
}

# pull_cmd RUNTIME IMAGE: pulls the image, prints seconds or "failed" and sets
# PULL_REASON to a one-line explanation when it fails.
PULL_REASON=""
pull_cmd() {
  local runtime="$1" image="$2" out
  case "$1" in
    minibox) timed "$BIN" pull "$2" ;;
    docker)  timed docker pull "$2" ;;
    podman)  timed podman pull "$2" ;;
  esac
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    case "$runtime" in
      minibox) out="$("$BIN" pull "$image" 2>&1 >/dev/null | tail -1)" ;;
      docker)  out="$(docker pull "$image" 2>&1 >/dev/null | tail -1)" ;;
      podman)  out="$(podman pull "$image" 2>&1 >/dev/null | tail -1)" ;;
    esac
    [ -n "$out" ] || out="no error output"
    PULL_REASON="$(printf '%s' "$out" | tr '|\n' '//' | cut -c1-70)"
  fi
}

# disk_cmd RUNTIME IMAGE: prints the image's size on disk.
disk_cmd() {
  case "$1" in
    minibox)
      local root="${MINIBOX_ROOT:-}"
      [ -n "$root" ] || { echo "-"; return; }
      du -sh "$root/blobs" "$root/layers" 2>/dev/null | awk '{printf "%s+%s", $1, $2}' | sed 's/+[^ ]*$//' || echo "-"
      ;;
    docker)
      docker image inspect --format '{{.Size}}' "$2" 2>/dev/null | awk '{printf "%.1f GB", $1/1000000000}' || echo "-"
      ;;
    podman)
      podman image inspect --format '{{.Size}}' "$2" 2>/dev/null | awk '{printf "%.1f GB", $1/1000000000}' || echo "-"
      ;;
  esac
}

# run_cmd RUNTIME IMAGE CMD...: prints the run command as a single string.
run_cmd() {
  case "$1" in
    minibox) shift; echo "$BIN run --rm --network host $*" ;;
    docker)  shift; echo "docker run --rm --network host $*" ;;
    podman)  shift; echo "podman run --rm --network host $*" ;;
  esac
}

# warm_run RUNTIME IMAGE: prints mean milliseconds of `run --rm IMAGE true`.
warm_run() {
  local runtime="$1" image="$2" cmd
  cmd="$(run_cmd "$runtime" "$image" "$image" true)"
  if command -v hyperfine >/dev/null 2>&1; then
    local json="bench/results/hyperfine-$runtime-$(echo "$image" | tr '/:' '__').json"
    if hyperfine --warmup "$WARMUP" --runs "$RUNS" --export-json "$json" -n "$runtime" "$cmd" >/dev/null 2>&1; then
      local ms
      ms="$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1]))["results"][0]; print("%.1f" % (d["mean"]*1000))' "$json" 2>/dev/null)"
      if [ -z "$ms" ] && command -v jq >/dev/null 2>&1; then
        ms="$(jq -r '.results[0].mean*1000 | .*10|round/10' "$json" 2>/dev/null)"
      fi
      if [ -n "$ms" ]; then echo "$ms"; return 0; fi
    fi
  fi
  # Fallback: a shell timing loop over the same command.
  local total=0 i start end rc ms
  for i in $(seq 1 "$RUNS"); do
    start="$(now)"
    bash -c "$cmd" >/dev/null 2>&1
    rc=$?
    end="$(now)"
    [ "$rc" -ne 0 ] && { echo "failed"; return 1; }
    ms="$(awk -v s="$start" -v e="$end" 'BEGIN { printf "%.0f", (e - s) * 1000 }')"
    total=$((total + ms))
  done
  awk -v t="$total" -v n="$RUNS" 'BEGIN { printf "%.1f", t / n }'
}

# fail_reason RUNTIME IMAGE: a one-line, table-safe reason why `run` fails,
# which is what makes a partially supported host diagnosable from the report.
fail_reason() {
  local runtime="$1" image="$2" cmd out
  cmd="$(run_cmd "$runtime" "$image" "$image" true)"
  # The first line carries the cause; later lines are often hints.
  out="$(bash -c "$cmd" 2>&1 >/dev/null | sed 's/^minibox: //' | head -1)"
  [ -n "$out" ] || out="no error output"
  printf '%s' "$out" | tr '|\n' '//' | cut -c1-70
}

# container_pid RUNTIME NAME: prints the host pid of a running container.
container_pid() {
  local pid
  case "$1" in
    minibox) "$BIN" inspect "$2" 2>/dev/null | grep -o '"pid": *[0-9]*' | grep -o '[0-9]*' | head -1 ;;
    docker)  docker inspect --format '{{.State.Pid}}' "$2" 2>/dev/null ;;
    podman)  podman inspect --format '{{.State.Pid}}' "$2" 2>/dev/null ;;
  esac | tr -d '[:space:]'
}

# stop_container RUNTIME NAME: stops and removes a container, best effort.
stop_container() {
  case "$1" in
    minibox) "$BIN" stop -t 5 "$2" >/dev/null 2>&1; "$BIN" rm -f "$2" >/dev/null 2>&1 ;;
    docker)  docker rm -f "$2" >/dev/null 2>&1 ;;
    podman)  podman rm -f "$2" >/dev/null 2>&1 ;;
  esac
}

# start_serving RUNTIME IMAGE NAME: starts the container detached, publishing
# the ollama port. Prints nothing; returns non-zero when the start fails.
START_REASON=""
start_serving() {
  local runtime="$1" image="$2" name="$3" out rc
  case "$runtime" in
    minibox) out="$("$BIN" run -d --name "$name" -p "$PORT:11434" "$image" 2>&1 >/dev/null)" ;;
    docker)  out="$(docker run -d --name "$name" -p "$PORT:11434" "$image" 2>&1 >/dev/null)" ;;
    podman)  out="$(podman run -d --name "$name" -p "$PORT:11434" "$image" 2>&1 >/dev/null)" ;;
  esac
  rc=$?
  if [ "$rc" -ne 0 ]; then
    [ -n "$out" ] || out="docker/podman returned no error output"
    START_REASON="$(printf '%s' "$out" | sed 's/^minibox: //' | head -1 | tr '|\n' '//' | cut -c1-70)"
  fi
  return "$rc"
}

# ollama_serving RUNTIME IMAGE: prints "seconds|rssMB" until /api/version answers.
ollama_serving() {
  local runtime="$1" image="$2" name="bench-ollama-$$"
  local start end
  start="$(now)"
  if ! start_serving "$runtime" "$image" "$name"; then
    echo "failed: ${START_REASON:-unknown error}|-"
    return 1
  fi
  trap 'stop_container '"$runtime"' '"$name"'' EXIT
  local deadline=$(( $(date +%s) + READY_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl -fsS "http://127.0.0.1:$PORT/api/version" >/dev/null 2>&1; then
      end="$(now)"
      local pid rss
      pid="$(container_pid "$runtime" "$name")"
      rss="-"
      if [ -n "$pid" ] && [ "$pid" != "0" ]; then
        rss="$(ps -o rss= -p "$pid" 2>/dev/null | awk '{printf "%.0f", $1/1024}')"
        [ -n "$rss" ] || rss="-"
      fi
      stop_container "$runtime" "$name"
      trap - EXIT
      awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }'
      echo "|$rss"
      return 0
    fi
    sleep 1
  done
  stop_container "$runtime" "$name"
  trap - EXIT
  echo "timeout|-"
  return 1
}

# daemon_rss: approximate idle memory of the container daemons, matched by exact
# process name so the benchmark's own command lines are never counted.
daemon_rss() {
  local total=0 pid rss
  for pid in $(pgrep -x dockerd 2>/dev/null) $(pgrep -x containerd 2>/dev/null) $(pgrep -x podman 2>/dev/null); do
    rss="$(ps -o rss= -p "$pid" 2>/dev/null)"
    [ -n "$rss" ] && total=$((total + rss))
  done
  awk -v t="$total" 'BEGIN { printf "%.0f", t/1024 }'
}

# disk_cmd2 RUNTIME IMAGE: image size on disk, one number plus a unit.
disk_cmd2() {
  case "$1" in
    minibox)
      local root="${MINIBOX_ROOT:-}"
      if [ -z "$root" ]; then echo "-"; return; fi
      local b l
      b="$(du -sh "$root/blobs" 2>/dev/null | cut -f1)"
      l="$(du -sh "$root/layers" 2>/dev/null | cut -f1)"
      if [ -n "$b" ] && [ -n "$l" ]; then echo "$b blobs + $l layers"; else echo "-"; fi
      ;;
    docker)
      docker image inspect --format '{{.Size}}' "$2" 2>/dev/null | awk '{printf "%.2f GB", $1/1000000000}' || echo "-"
      ;;
    podman)
      podman image inspect --format '{{.Size}}' "$2" 2>/dev/null | awk '{printf "%.2f GB", $1/1000000000}' || echo "-"
      ;;
  esac
}

# ------------------------------------------------------------------ tables ---
say "| runtime | image | pull | warm run | on disk |"
say "|---|---|---|---|---|"

for image in "${IMAGES[@]}"; do
  root="${MINIBOX_ROOT:-}"
  if [ -z "$root" ]; then
    root="$(mktemp -d /tmp/minibox-compare-XXXXXX)"
    export MINIBOX_ROOT="$root"
    OWN_ROOT=1
  else
    OWN_ROOT=0
  fi

  is_ollama=0
  case "$image" in *ollama*) is_ollama=1;; esac

  for runtime in "${AVAILABLE[@]}"; do
    if [ "$SKIP_PULL" = "1" ]; then pull="-"; else
      pull="$(pull_cmd "$runtime" "$image")"
      [ -n "$pull" ] || pull="failed"
    fi
    disk="$(disk_cmd2 "$runtime" "$image")"
    [ -n "$disk" ] || disk="-"

    warm="-"
    if [ "$PULL_ONLY" != "1" ] && [ "$is_ollama" = "0" ]; then
      if [ "$pull" != "failed" ]; then
        warm="$(warm_run "$runtime" "$image")"
        if [ -z "$warm" ] || [ "$warm" = "failed" ]; then
          warm="failed: $(fail_reason "$runtime" "$image")"
        else
          warm="${warm} ms"
        fi
      fi
    fi
    [ "$is_ollama" = "1" ] && warm="n/a (server image)"

    case "$pull" in
      failed) pull="failed: ${PULL_REASON:-unknown error}" ;;
      -) ;;
      *) pull="$pull s" ;;
    esac
    say "| $runtime | $image | $pull | $warm | $disk |"
  done

  if [ "$OWN_ROOT" = "1" ]; then
    rm -rf "$root"
    unset MINIBOX_ROOT
  fi
done

say ""

# --------------------------------------------------- ollama serving + memory ---
if [ "$PULL_ONLY" != "1" ]; then
  ollama_image=""
  for image in "${IMAGES[@]}"; do
    case "$image" in *ollama*) ollama_image="$image";; esac
  done
  if [ -n "$ollama_image" ]; then
    # The image loop may have cleaned up its state root; minibox needs one.
    if [ -z "${MINIBOX_ROOT:-}" ]; then
      export MINIBOX_ROOT="$(mktemp -d /tmp/minibox-compare-XXXXXX)"
      SERVE_ROOT="$MINIBOX_ROOT"
    fi
    say "## ollama: time to serving and memory"
    say ""
    say "| runtime | time to /api/version | container RSS |"
    say "|---|---|---|"
    for runtime in "${AVAILABLE[@]}"; do
      if [ "$SKIP_PULL" != "1" ]; then
        pull_cmd "$runtime" "$ollama_image" >/dev/null
      fi
      result="$(ollama_serving "$runtime" "$ollama_image")"
      secs="${result%%|*}"
      rss="${result##*|}"
      [ -n "$secs" ] || secs="failed"
      case "$secs" in
        failed*|timeout*) say "| $runtime | $secs | - |" ;;
        *) say "| $runtime | ${secs} s | ${rss} MB |" ;;
      esac
    done
    say ""
    if [ -n "${SERVE_ROOT:-}" ]; then
      rm -rf "$SERVE_ROOT"
      unset MINIBOX_ROOT SERVE_ROOT
    fi
  fi
fi

say "## runtime background memory (approximate)"
say ""
say "| runtime | idle daemon/shim RSS |"
say "|---|---|"
say "| minibox | 0 MB (no daemon; detached containers are supervised by a short-lived shim) |"
say "| docker+podman processes on this host | $(daemon_rss) MB |"
say ""

echo "report: $REPORT"

if [ "${BENCH_APPEND:-0}" = "1" ]; then
  {
    echo
    echo "### $(date -u +%Y-%m-%dT%H:%M:%SZ) - three-runtime comparison"
    echo
    cat "$REPORT"
  } >> docs/BENCHMARKS.md
  echo "appended to docs/BENCHMARKS.md"
fi

if [ "${#AVAILABLE[@]}" -lt 2 ]; then
  echo >&2
  echo "warning: only ${#AVAILABLE[@]} runtime(s) could be measured, so this is not a comparison." >&2
  for m in "${MISSING[@]}"; do echo "  - $m" >&2; done
  exit 1
fi
exit 0
