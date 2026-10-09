#!/usr/bin/env bash
# Run the integration suite on a machine that can create namespaces and mount
# overlayfs (a VM, a bare-metal Linux box, or a privileged container).
#
# The suite needs: Linux 5.7+ with cgroup v2, root (or user namespaces with
# unprivileged overlayfs on 5.11+), and network access for image pulls.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ "$(id -u)" -ne 0 ]; then
  echo "warning: not running as root; only the rootless parts of the suite will pass" >&2
fi

if [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
  echo "error: cgroup v2 is required (/sys/fs/cgroup/cgroup.controllers is missing)" >&2
  echo "       boot with systemd.unified_cgroup_hierarchy=1" >&2
  exit 1
fi

ROOT="${MINIBOX_ROOT:-$(mktemp -d /tmp/minibox-it-XXXXXX)}"
export MINIBOX_ROOT="$ROOT"
echo "state root: $ROOT"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/minibox ./cmd/minibox
export MINIBOX_BIN="$PWD/bin/minibox"

go test -tags integration -count=1 -v -timeout 30m ./test/integration/ "$@"

echo
echo "leftover state check:"
"$MINIBOX_BIN" ps -a || true
echo "mounts under $ROOT:"
grep -c "$ROOT" /proc/self/mountinfo || echo 0

