#!/usr/bin/env bash
# Build a minimal OCI bundle so `crun run bench-bundle` works as a floor
# measurement for bench/run.sh. Needs root and an unpacked rootfs.
set -euo pipefail

ROOT="${1:-$(mktemp -d /tmp/minibox-bundle-XXXXXX)}"
ROOTFS="$ROOT/rootfs"
BUNDLE="$ROOT/bundle"

if [ ! -d "$ROOTFS" ]; then
  echo "usage: make-bundle.sh [directory containing an unpacked rootfs]" >&2
  echo "hint: unpack an alpine layer first, for example with" >&2
  echo "      mkdir -p $ROOTFS && tar -C $ROOTFS -xzf layer.tar.gz" >&2
  exit 1
fi

mkdir -p "$BUNDLE"
cat > "$BUNDLE/config.json" <<EOF
{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 0, "gid": 0},
    "args": ["/bin/true"],
    "env": ["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],
    "cwd": "/"
  },
  "root": {"path": "$ROOTFS", "readonly": false},
  "hostname": "bench",
  "linux": {
    "namespaces": [
      {"type": "pid"}, {"type": "mount"}, {"type": "ipc"},
      {"type": "uts"}, {"type": "network"}
    ]
  }
}
EOF
echo "bundle ready: $BUNDLE (use ROOT=$ROOT when running bench/run.sh)"

