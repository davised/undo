#!/usr/bin/env bash
# Assertions for test/multifs.sh: doctor must answer per filesystem.
#
#   test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-doctor.sh'
#
# FS_A and FS_B are exported by the harness and are separate tmpfs mounts with
# distinct st_dev.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
UNDO=$ROOT/bin/undo
export UNDO_LIB=$ROOT/build/libundo.so
export UNDO_DATA_DIR=${UNDO_DATA_DIR:-/tmp/undo-multifs-store}
export UNDO_HOOK=multifs   # doctor treats a missing hook as a failure

fail() { echo "FAIL: $*" >&2; exit 1; }

storeroot() { # storeroot <doctor output>
    sed -n 's/.*store \([^;]*\);.*/\1/p' <<<"$1" | head -1
}

mkdir -p "$FS_A/work" "$FS_B/work"

echo "== multifs: doctor reports a different store root per filesystem"
a=$("$UNDO" doctor "$FS_A/work" 2>&1) || fail "doctor failed on the first filesystem: $a"
b=$("$UNDO" doctor "$FS_B/work" 2>&1) || fail "doctor failed on the second: $b"
roota=$(storeroot "$a")
rootb=$(storeroot "$b")
[[ -n $roota ]] || fail "no store root reported for the first filesystem: $a"
[[ -n $rootb ]] || fail "no store root reported for the second: $b"
[[ $roota != "$rootb" ]] ||
    fail "both filesystems reported the same store root ($roota); placement is not per-filesystem"

echo "== multifs: both store roots lie on the filesystem they describe"
[[ $roota == "$FS_A"* ]] || fail "first store root $roota is not under $FS_A"
[[ $rootb == "$FS_B"* ]] || fail "second store root $rootb is not under $FS_B"

echo
echo "multifs doctor assertions ok"
