#!/usr/bin/env bash
# Cross-device restore: the session store on one filesystem, the files on
# another, so every os.Rename in the restore path genuinely returns EXDEV.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-restore.sh'
#
# Sourced by test/multifs.sh, which exports FS_A and FS_B.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
[[ -x $UNDO ]] || fail "bin/undo not built; run make first"

# store on B, files on A: every restore crosses a filesystem boundary
export UNDO_DATA_DIR=$FS_B/undo-data
WORK=$FS_A/work
mkdir -p "$WORK" "$UNDO_DATA_DIR"

# Build each session by hand rather than through the shim: this is a Go-side
# test, and driving it directly keeps it independent of shim behavior.
#
# `undo <id>` is not a command -- a bare argument falls through to the usage
# text and exit 2. The spellings below (apply / redo / purge) are the real ones.
n=0
new_session() { # new_session <journal-line...>  -> sets $sdir
    n=$((n + 1))
    sid=$(printf '17%08d%06d' "$n" "$n")
    sdir=$UNDO_DATA_DIR/sessions/$sid
    mkdir -p "$sdir/data"
    printf 'synthetic cross-device session %s\n' "$n" >"$sdir/cmd"
    : >"$sdir/done"
}

echo "== cross-device: a created symlink survives undo and redo as a symlink"
new_session
ln -s /etc/hostname "$WORK/alink"
printf 'create\t%s\n' "$WORK/alink" >"$sdir/journal"
"$UNDO" apply "$sid" -y >/dev/null || fail "undo of the created symlink failed"
[[ ! -e $WORK/alink && ! -L $WORK/alink ]] ||
    fail "undo did not remove the symlink"
[[ -L $sdir/data/undo-0 ]] ||
    fail "a symlink parked across filesystems is no longer a symlink"
"$UNDO" redo "$sid" -y >/dev/null || fail "redo of the created symlink failed"
[[ -L $WORK/alink ]] || fail "a symlink restored across filesystems is not a symlink"
[[ $(readlink "$WORK/alink") == /etc/hostname ]] ||
    fail "restored symlink points at $(readlink "$WORK/alink")"

echo "== cross-device: an executable keeps its mode"
new_session
printf '#!/bin/sh\n' >"$sdir/data/b2"
chmod 755 "$sdir/data/b2"
printf 'unlink\t%s\t%s\n' "$WORK/script" "$sdir/data/b2" >"$sdir/journal"
"$UNDO" apply "$sid" -y >/dev/null || fail "undo of the script failed"
mode=$(stat -c %a "$WORK/script")
[[ $mode == 755 ]] || fail "mode across filesystems = $mode, want 755"

echo "== cross-device: undo --force parks a populated directory"
new_session
mkdir -p "$WORK/made/sub"
echo "do not lose me" >"$WORK/made/sub/kept.txt"
printf 'mkdir\t%s\n' "$WORK/made" >"$sdir/journal"
"$UNDO" apply "$sid" -y --force >/dev/null || fail "forced undo of the mkdir failed"
[[ ! -e $WORK/made ]] || fail "the directory was not moved aside"
[[ $(cat "$sdir/data/undo-0/sub/kept.txt") == "do not lose me" ]] ||
    fail "the parked tree lost its contents crossing a filesystem"

echo "== cross-device: redo brings the directory back"
"$UNDO" redo "$sid" -y >/dev/null || fail "redo failed"
[[ $(cat "$WORK/made/sub/kept.txt") == "do not lose me" ]] ||
    fail "the tree did not come back intact"

echo "== cross-device: purge reclaims a store on the other filesystem"
new_session
store=$FS_A/.undo/$sid
mkdir -p "$store"
echo saved >"$store/1-1"
printf 'unlink\t%s\t%s\n' "$WORK/gone.txt" "$store/1-1" >"$sdir/journal"
"$UNDO" purge -y >/dev/null || fail "purge failed"
[[ ! -e $store/1-1 ]] || fail "purge left a backup on the other filesystem"
[[ ! -e $FS_A/.undo ]] || fail "purge left an empty .undo behind"

echo
echo "cross-device restore ok"
