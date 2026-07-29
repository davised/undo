#!/usr/bin/env bash
# Run a command inside a Linux container against a copy of the working tree.
# The shim is Linux-only, so this is how it gets built and tested from macOS.
#
#   test/in-container.sh make test
#   test/in-container.sh --privileged test/multifs.sh
set -euo pipefail

IMAGE=${UNDO_TEST_IMAGE:-docker.io/library/golang:1.24-bookworm}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

engine=""
for e in podman docker; do
    command -v "$e" >/dev/null 2>&1 && { engine=$e; break; }
done
[ -n "$engine" ] || { echo "in-container: need podman or docker" >&2; exit 1; }

opts=()
if [ "${1-}" = "--privileged" ]; then
    opts+=(--privileged)
    shift
fi
[ $# -gt 0 ] || { echo "usage: in-container.sh [--privileged] <cmd>..." >&2; exit 2; }

# The tree is mounted read-only and copied in: tests write freely without
# touching the host checkout, and a failed run leaves nothing behind.
# ${opts[@]+...} guards the empty-array case: bash 3.2, which macOS still
# ships as /bin/bash, treats a bare "${opts[@]}" as unset under `set -u`.
exec "$engine" run --rm ${opts[@]+"${opts[@]}"} -v "$ROOT":/src:ro -w / "$IMAGE" bash -c '
  set -e
  mkdir -p /w && cp -r /src/. /w/ && cd /w
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null 2>&1
  apt-get install -y -qq gcc make >/dev/null 2>&1
  '"$(printf '%q ' "$@")"'
'
