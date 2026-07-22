#!/usr/bin/env bash
# End-to-end test: arms the shim the same way the zsh hook does, wrecks a
# directory tree, then checks that `undo` puts everything back.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
UNDO=$ROOT/bin/undo
LIB=$ROOT/build/libundo.so
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

export UNDO_DATA_DIR=$WORK/store
PLAY=$WORK/play

fail() { echo "FAIL: $*" >&2; exit 1; }

run_armed() {
    local id
    id=$(date +%s%N | cut -c1-16)
    local sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "$*"
    sleep 0.01 # keep session ids strictly ordered
}

make_tree() {
    rm -rf "$PLAY"
    mkdir -p "$PLAY/docs/sub"
    echo "report v1" >"$PLAY/docs/report.txt"
    echo "deep note" >"$PLAY/docs/sub/note.txt"
    echo "top file" >"$PLAY/top.txt"
    ln -s top.txt "$PLAY/lnk"
    chmod 640 "$PLAY/docs/report.txt"
}

echo "== case 1: rm -rf a tree"
make_tree
cp -a "$PLAY" "$WORK/expected"
run_armed "rm -rf $PLAY/docs"
[[ ! -e $PLAY/docs ]] || fail "rm -rf did not run"
"$UNDO" -y
diff -r "$PLAY" "$WORK/expected" || fail "tree not restored after rm -rf"
[[ $(stat -c %a "$PLAY/docs/report.txt") == 640 ]] || fail "mode lost"

echo "== case 2: clobbered by redirection"
run_armed "echo garbage > $PLAY/top.txt"
[[ $(cat "$PLAY/top.txt") == garbage ]] || fail "redirect did not run"
"$UNDO" -y
[[ $(cat "$PLAY/top.txt") == "top file" ]] || fail "content not restored"

echo "== case 3: mv over an existing file"
run_armed "mv $PLAY/top.txt $PLAY/docs/report.txt"
[[ $(cat "$PLAY/docs/report.txt") == "top file" ]] || fail "mv did not run"
"$UNDO" -y
[[ $(cat "$PLAY/top.txt") == "top file" ]] || fail "moved file not back"
[[ $(cat "$PLAY/docs/report.txt") == "report v1" ]] || fail "target not back"

echo "== case 4: created files and dirs are removed"
run_armed "mkdir -p $PLAY/junk && touch $PLAY/junk/a.txt $PLAY/stray.txt"
"$UNDO" -y
[[ ! -e $PLAY/junk && ! -e $PLAY/stray.txt ]] || fail "creations not removed"

echo "== case 5: deleted symlink comes back"
run_armed "rm $PLAY/lnk"
"$UNDO" -y
[[ $(readlink "$PLAY/lnk") == top.txt ]] || fail "symlink not restored"

echo "== case 6: undone sessions are skipped, dry run touches nothing"
run_armed "rm $PLAY/top.txt"
"$UNDO" -n >/dev/null
[[ ! -e $PLAY/top.txt ]] || fail "dry run restored the file"
"$UNDO" -y
[[ -e $PLAY/top.txt ]] || fail "file not restored"
out=$("$UNDO" -y 2>&1 || true)
grep -q "nothing to undo" <<<"$out" || fail "expected nothing to undo, got: $out"

echo "== case 7: refuses to clobber without --force"
run_armed "rm $PLAY/top.txt"
echo "newer content" >"$PLAY/top.txt"
"$UNDO" -y 2>&1 | grep -q "skipped" || fail "expected a skip warning"
[[ $(cat "$PLAY/top.txt") == "newer content" ]] || fail "clobbered without force"

echo "== case 8: undo run arms the shim without a hook"
export UNDO_LIB=$LIB
"$UNDO" run -- rm "$PLAY/top.txt" 2>&1 | grep -q "captured 1 change" || fail "run did not capture"
[[ ! -e $PLAY/top.txt ]] || fail "run did not execute rm"
"$UNDO" -y
[[ $(cat "$PLAY/top.txt") == "newer content" ]] || fail "run session not undoable"

echo "== case 9: redo re-applies, then undo works again"
run_armed "rm $PLAY/top.txt"
"$UNDO" -y
[[ -e $PLAY/top.txt ]] || fail "undo failed"
"$UNDO" redo -y
[[ ! -e $PLAY/top.txt ]] || fail "redo did not re-delete"
"$UNDO" -y
[[ $(cat "$PLAY/top.txt") == "newer content" ]] || fail "second undo failed"

echo "== case 10: mod entries toggle both ways without losing either version"
echo "original" >"$PLAY/toggle.txt"
run_armed "echo overwritten > $PLAY/toggle.txt"
"$UNDO" -y
[[ $(cat "$PLAY/toggle.txt") == "original" ]] || fail "undo lost original"
"$UNDO" redo -y
[[ $(cat "$PLAY/toggle.txt") == "overwritten" ]] || fail "redo lost new version"
"$UNDO" -y
[[ $(cat "$PLAY/toggle.txt") == "original" ]] || fail "second undo failed"

echo "== case 11: interactive cherry-pick restores only selected entries"
echo "one" >"$PLAY/f1.txt"
echo "two" >"$PLAY/f2.txt"
run_armed "rm $PLAY/f1.txt $PLAY/f2.txt"
printf '1\n1\ny\n' | "$UNDO" -i >/dev/null
[[ -e $PLAY/f1.txt && ! -e $PLAY/f2.txt ]] || fail "cherry-pick restored wrong set"
"$UNDO" -y 2>/dev/null
[[ -e $PLAY/f1.txt && -e $PLAY/f2.txt ]] || fail "full undo after cherry-pick failed"

echo "== case 12: diff shows content changes"
echo "alpha" >"$PLAY/d.txt"
run_armed "echo beta > $PLAY/d.txt"
out=$("$UNDO" diff)
grep -q -- "-alpha" <<<"$out" || fail "diff missing removed line"
grep -q -- "+beta" <<<"$out" || fail "diff missing added line"

echo
echo "all cases passed"
