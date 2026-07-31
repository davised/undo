#!/usr/bin/env bash
# The orphan sweep reclaiming a store on a filesystem other than the one
# holding the session directory.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-gc.sh'
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
LIB=$ROOT/build/libundo.so
[[ -x $UNDO && -f $LIB ]] || fail "run make first"

export UNDO_DATA_DIR=$FS_B/undo-data
mkdir -p "$UNDO_DATA_DIR"

run_armed() {
    local id sess
    id=$(date +%s%N | cut -c1-16)
    sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "$*"
    sleep 0.01
}

echo "== a store on the other filesystem is registered"
mkdir -p "$FS_A/user/work"
echo content >"$FS_A/user/work/f.txt"
run_armed "rm $FS_A/user/work/f.txt"
"$UNDO" gc >/dev/null
grep -q "^$FS_A" "$UNDO_DATA_DIR/roots" ||
    fail "the store root on $FS_A was not registered: $(cat "$UNDO_DATA_DIR/roots" 2>&1)"

echo "== an orphaned store is reclaimed"
undodir=$(find "$FS_A" -type d -name .undo | head -1)
[[ -n $undodir ]] || fail "no .undo directory was created on $FS_A"
orphan=$undodir/1700000000000001
mkdir -p "$orphan"
echo stranded >"$orphan/1-1"
"$UNDO" gc >/dev/null
[[ ! -e $orphan ]] || fail "the orphaned store was not reclaimed"

echo "== a live session's store is left alone"
live=$(ls -1 "$UNDO_DATA_DIR/sessions" | sort | tail -1)
[[ -d $undodir/$live ]] || fail "the sweep removed a live session's store"

echo
echo "orphan sweep ok"
