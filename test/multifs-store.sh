#!/usr/bin/env bash
# Store placement on two real filesystems: the session directory on one, the
# files on the other, so a backup that is NOT filesystem-local is provably a
# cross-device copy rather than a hardlink.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-store.sh'
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
LIB=$ROOT/build/libundo.so
[[ -x $UNDO && -f $LIB ]] || fail "run make first"

export UNDO_DATA_DIR=$FS_B/undo-data
mkdir -p "$UNDO_DATA_DIR"

# Arms the shim exactly the way test/e2e.sh does, so the two suites agree on
# what "armed" means. Takes one string, run through bash -c.
run_armed() {
    local id sess
    id=$(date +%s%N | cut -c1-16)
    sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "$*"
    sleep 0.01 # keep session ids strictly ordered
}

latest() { ls -1d "$UNDO_DATA_DIR"/sessions/* | sort | tail -1; }

echo "== the store lands on the file's own filesystem"
mkdir -p "$FS_A/user/project"
echo "content" >"$FS_A/user/project/f.txt"
run_armed "rm $FS_A/user/project/f.txt"
sess=$(latest)
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | tail -1)
[[ $bak == "$FS_A"/* ]] || fail "backup landed at $bak, not on $FS_A"
[[ $bak == *"/.undo/"* ]] || fail "backup is not inside a .undo store: $bak"

echo "== a deletion is hardlinked, not copied"
method=$(awk -F'\t' '$1=="unlink"{print $4}' "$sess/journal" | tail -1)
[[ $method == link ]] || fail "save method = '$method', want link"

echo "== the store is not inside the tree that was operated on"
case $bak in
    "$FS_A"/user/project/*) fail "the store is inside the operated tree: $bak" ;;
esac

echo "== rm -rf of the store's own directory still succeeds"
mkdir -p "$FS_A/user/tree/sub"
echo "recoverable" >"$FS_A/user/tree/sub/x.txt"
run_armed "rm -rf $FS_A/user/tree"
[[ ! -e $FS_A/user/tree ]] || fail "rm -rf left the tree behind"

echo "== and the backup survived the evacuation"
sess=$(latest)
"$UNDO" apply "$(basename "$sess")" -y >/dev/null || fail "undo failed"
[[ $(cat "$FS_A/user/tree/sub/x.txt") == "recoverable" ]] ||
    fail "the backup did not survive"

echo "== purge reclaims the distributed store"
"$UNDO" purge -y >/dev/null
found=$(find "$FS_A" -name '.undo' -type d 2>/dev/null | head -1)
[[ -z $found ]] || fail "purge left a store behind at $found"

echo "== an unwritable store never fails the user's command"
mkdir -p "$FS_A/ro/sub"
echo data >"$FS_A/ro/sub/f.txt"
chmod 500 "$FS_A/ro"
if ! run_armed "rm $FS_A/ro/sub/f.txt"; then
    chmod 700 "$FS_A/ro"
    fail "rm failed because the store was unwritable"
fi
chmod 700 "$FS_A/ro"
[[ ! -e $FS_A/ro/sub/f.txt ]] || fail "the rm did not actually happen"

echo "== a symlinked store is refused rather than written through"
# On shared storage the store root is the user's own directory but may still be
# group-writable, so another user can pre-create .undo -- or the entirely
# predictable .undo/<session-id> -- as a symlink. mkdir then returns EEXIST and
# every backup, which is a copy of a file the user just deleted, would be
# written wherever that link points.
rm -rf "$FS_A/.undo"
rm -rf /tmp/attacker-dir && mkdir -p /tmp/attacker-dir
ln -s /tmp/attacker-dir "$FS_A/.undo"
mkdir -p "$FS_A/sym"
echo secret >"$FS_A/sym/victim.txt"
run_armed "rm $FS_A/sym/victim.txt"
[[ -z $(ls -A /tmp/attacker-dir) ]] ||
    fail "a backup was written through a symlinked store: $(ls -A /tmp/attacker-dir)"
sess=$(latest)
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | tail -1)
[[ $bak == "$UNDO_DATA_DIR"/* ]] ||
    fail "expected the session-directory fallback, got $bak"
rm -f "$FS_A/.undo"

echo
echo "store placement ok"
