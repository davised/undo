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
# run_armed does what a shell hook does, so declare the hook active:
# doctor treats a missing hook as a failure, since nothing gets recorded
export UNDO_HOOK=e2e
PLAY=$WORK/play

fail() { echo "FAIL: $*" >&2; exit 1; }

# Field <n> of /proc/<pid>/stat, one-indexed.
#
# Everything up to the LAST ')' is stripped first: field 2 is the executable
# name in parentheses and may contain spaces and parentheses of its own, so
# `cut -d' ' -fN` silently reads the wrong field for such a process.
statfield() { # statfield <pid|self> <n>
    sed 's/.*) //' "/proc/$1/stat" | awk -v n="$(( $2 - 2 ))" '{print $n}'
}

# UNDO_ARM exactly as `undo arm` builds it: the process group's id, paired with
# the START TIME OF THE GROUP LEADER -- not our own.
#
# When this shell is not its group leader, which is the ordinary case in a
# container with no job control, those are two different processes and pairing
# them describes nothing at all. armer_is_us would never match and the
# exclusion would silently stop working.
#
# Falls back to the static "1" exactly as cmdArm does, rather than failing.
# A group whose leader has already exited has no /proc/<pgid>/stat, and
# returning nothing would expand to UNDO_ARM= -- an empty value, which is a
# third state neither the shim nor this harness should have to reason about.
arm_id() {
    local pgid st
    pgid=$(statfield self 5)
    st=""
    [[ -n $pgid ]] && st=$(statfield "$pgid" 22)
    if [[ -z $pgid || -z $st ]]; then
        printf '1\n'
        return 0
    fi
    printf '%s:%s\n' "$pgid" "$st"
}

run_armed() {
    local id
    id=$(date +%s%N | cut -c1-16)
    local sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    # a hook records which kernel instance made the session; so must this,
    # or no e2e case ever exercises the same-host path with an origin present
    printf '%s\t%s\t%s\n' "$(uname -n)" \
        "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
        "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
    env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
        LD_PRELOAD="$LIB" bash -c "$*"
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
# Captured rather than piped into grep -q, and it needs to stay that way. undo
# prints the skip lines and then "restored N change(s)"; grep -q exits at the
# first match, so that trailing write lands on a closed pipe, Go re-raises
# SIGPIPE on stdout, and undo dies 141. Under `set -o pipefail` that fails the
# pipeline even though grep matched -- an intermittent failure of an assertion
# that actually passed, seen about once in 25 runs.
out=$("$UNDO" -y 2>&1 || true)
grep -q "skipped" <<<"$out" || fail "expected a skip warning, got: $out"
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

echo "== case 13: chmod is journaled and reversible"
chmod 644 "$PLAY/d.txt"
run_armed "chmod 600 $PLAY/d.txt"
[[ $(stat -c %a "$PLAY/d.txt") == 600 ]] || fail "chmod did not run"
"$UNDO" -y
[[ $(stat -c %a "$PLAY/d.txt") == 644 ]] || fail "mode not restored"
"$UNDO" redo -y
[[ $(stat -c %a "$PLAY/d.txt") == 600 ]] || fail "mode redo failed"

echo "== case 14: refuses to undo a session whose command may be running"
run_armed "rm $PLAY/f2.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
echo $$ >"$UNDO_DATA_DIR/sessions/$last/pid"
rm -f "$UNDO_DATA_DIR/sessions/$last/done"
out=$("$UNDO" -y 2>&1 || true)
grep -q "still be running" <<<"$out" || fail "live session not refused"
[[ ! -e $PLAY/f2.txt ]] || fail "live session was restored anyway"
touch "$UNDO_DATA_DIR/sessions/$last/done"
"$UNDO" -y
[[ -e $PLAY/f2.txt ]] || fail "undo failed after done marker"

echo "== case 15: gc removes empty sessions, purge empties the store"
mkdir -p "$UNDO_DATA_DIR/sessions/1111111111111111/data"
"$UNDO" gc | grep -q "removed" || fail "gc did not report"
[[ ! -d $UNDO_DATA_DIR/sessions/1111111111111111 ]] || fail "empty session survived gc"
"$UNDO" purge -y >/dev/null
[[ -z $(ls "$UNDO_DATA_DIR/sessions" 2>/dev/null) ]] || fail "purge left sessions"
out=$("$UNDO" -y 2>&1 || true)
grep -q "nothing to undo" <<<"$out" || fail "store not empty after purge"

echo "== case 16: default ignores skip build noise, real files still tracked"
mkdir -p "$PLAY/node_modules/pkg" "$PLAY/.cache" "$PLAY/src"
echo junk >"$PLAY/node_modules/pkg/i.js"
echo blob >"$PLAY/.cache/x"
echo real >"$PLAY/src/keep.c"
run_armed "rm $PLAY/node_modules/pkg/i.js $PLAY/.cache/x $PLAY/src/keep.c"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
j="$UNDO_DATA_DIR/sessions/$last/journal"
[[ $(grep -c . "$j") == 1 ]] || fail "expected 1 journal entry, got $(grep -c . "$j")"
grep -q "src/keep.c" "$j" || fail "real file not journaled"
grep -q "node_modules\|.cache/x" "$j" && fail "ignored path was journaled"
"$UNDO" -y >/dev/null
[[ -e $PLAY/src/keep.c ]] || fail "real file not restored"

echo "== case 17: UNDO_IGNORE adds custom patterns"
echo data >"$PLAY/scratch.tmp"
id=$(date +%s%N | cut -c1-16); sess="$UNDO_DATA_DIR/sessions/$id"
mkdir -p "$sess/data"; echo "rm scratch" >"$sess/cmd"
env UNDO_IGNORE="scratch.tmp" UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "rm $PLAY/scratch.tmp"
[[ ! -s $sess/journal ]] || fail "custom UNDO_IGNORE pattern was not honored"

echo "== case 18: repeated writes to one file keep a single backup"
echo original >"$PLAY/churn.txt"
run_armed "for i in 1 2 3 4 5; do echo edit\$i > $PLAY/churn.txt; done"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sd="$UNDO_DATA_DIR/sessions/$last"
[[ $(grep -c "churn.txt" "$sd/journal") == 1 ]] || fail "expected 1 mod entry, got $(grep -c churn.txt "$sd/journal")"
# Counted where the journal says the backup is, not in $sess/data: the store
# is per-filesystem now, so a backup normally lands in <root>/.undo/<id>/ and
# only falls back to the session directory. The store directory is per session,
# so its entry count is the same assertion as before.
bak=$(awk -F'\t' '$1=="mod"{print $3}' "$sd/journal" | tail -1)
[[ -n $bak && -f $bak ]] || fail "the backup named by the journal is missing: '$bak'"
n=$(ls "$(dirname "$bak")" | wc -l)
[[ $n == 1 ]] || fail "expected 1 backup, got $n"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/churn.txt") == original ]] || fail "dedup broke restore, got $(cat "$PLAY/churn.txt")"

echo "== case 19: truncate is caught even from a large-file build"
# anything compiled with _FILE_OFFSET_BITS=64 calls truncate64, which is
# most software; missing that symbol meant silent, unrecoverable truncation
printf 'keep me\n' >"$PLAY/trunc.txt"
cat >"$WORK/tr.c" <<'CEOF'
#include <unistd.h>
int main(int c, char **v) { (void)c; return truncate(v[1], 0); }
CEOF
if cc -D_FILE_OFFSET_BITS=64 -o "$WORK/tr64" "$WORK/tr.c" 2>/dev/null; then
    run_armed "$WORK/tr64 $PLAY/trunc.txt"
    [[ ! -s $PLAY/trunc.txt ]] || fail "truncate did not run"
    "$UNDO" -y >/dev/null
    [[ $(cat "$PLAY/trunc.txt") == "keep me" ]] || fail "truncate64 not restored"
else
    echo "   (no cc, skipped)"
fi

echo "== case 20: a failed rmdir does not stop the rest of the command being recorded"
mkdir -p "$PLAY/full/x" "$PLAY/gone"
run_armed "rmdir $PLAY/full $PLAY/gone || true"
[[ -d $PLAY/full && ! -d $PLAY/gone ]] || fail "rmdir did not run as expected"
"$UNDO" -y >/dev/null
[[ -d $PLAY/gone ]] || fail "second rmdir was not journaled after the first failed"

# One long-lived process writing the same file under two sessions in a
# row: what UNDO_CAPTURE_SHELL=1 does, where the shim is loaded into the
# shell itself rather than into a fresh child per command.
if command -v python3 >/dev/null 2>&1; then
    echo "== case 21: one process spanning two sessions backs up in both"
    f=$PLAY/shared.txt
    echo v0 >"$f"
    s1=$UNDO_DATA_DIR/sessions/$(date +%s%N | cut -c1-16); mkdir -p "$s1/data"
    echo "write v1" >"$s1/cmd"; sleep 0.01
    s2=$UNDO_DATA_DIR/sessions/$(date +%s%N | cut -c1-16); mkdir -p "$s2/data"
    echo "write v2" >"$s2/cmd"
    LD_PRELOAD="$LIB" python3 -c "
import os, sys
for sess, body in ((sys.argv[1], 'v1'), (sys.argv[2], 'v2')):
    os.environ['UNDO_SESSION'] = sess
    with open(sys.argv[3], 'w') as fh:
        fh.write(body + '\n')
" "$s1" "$s2" "$f"
    [[ $(cat "$f") == v2 ]] || fail "writes did not run"
    grep -q "shared.txt" "$s2/journal" 2>/dev/null || fail "second session recorded nothing"
    "$UNDO" -y >/dev/null
    [[ $(cat "$f") == v1 ]] || fail "second session did not restore v1, got $(cat "$f")"
else
    echo "== case 21: skipped (no python3)"
fi

echo "== case 22: undo run reports a signal death the way a shell does"
set +e
"$UNDO" run -- sh -c 'kill -TERM $$' >/dev/null 2>&1
rc=$?
set -e
[[ $rc == 143 ]] || fail "expected 143 from a SIGTERM death, got $rc"

echo "== case 23: undo doctor passes its live self-test"
out=$("$UNDO" doctor 2>&1) || fail "doctor exited non-zero: $out"
grep -q "\[ok  \] capture" <<<"$out" || fail "doctor capture check did not pass"
grep -q "\[ok  \] restore" <<<"$out" || fail "doctor restore check did not pass"

echo "== case 24: the rmdir guard leak does not cross into other operations"
# Complements case 20, which covers rmdir -> rmdir. This covers rmdir ->
# unlink: the reentrancy guard leaking out of a failed rmdir silenced every
# later operation in that process, not just later rmdirs.
#
# This has to be one compiled program: rmdir(1) and rm(1) are separate
# processes and the flag is thread-local, so a shell-based test would pass
# whether or not the bug is present.
mkdir -p "$PLAY/guard/sub"
touch "$PLAY/guard/sub/x"
echo "precious" >"$PLAY/guard/keep.txt"
cat >"$WORK/guard.c" <<'CEOF'
#include <unistd.h>
/* rmdir fails with ENOTEMPTY, then the unlink must still be journaled */
int main(int c, char **v) { (void)c; rmdir(v[1]); return unlink(v[2]); }
CEOF
if cc -o "$WORK/guard" "$WORK/guard.c" 2>/dev/null; then
    run_armed "$WORK/guard $PLAY/guard $PLAY/guard/keep.txt"
    [[ ! -e $PLAY/guard/keep.txt ]] || fail "the unlink did not run"
    # When the guard leaks there is no session at all, so undo exits non-zero
    # with "nothing to undo". Tolerate that here so the assertion below is
    # what reports the failure, rather than set -e aborting first.
    "$UNDO" -y >/dev/null 2>&1 || true
    [[ -e $PLAY/guard/keep.txt && $(cat "$PLAY/guard/keep.txt") == precious ]] ||
        fail "an unlink after a failed rmdir was not captured"
else
    echo "   (no cc, skipped)"
fi

echo "== case 25: a deletion is hardlinked into a store beside the file"
mkdir -p "$PLAY/store-local"
echo "large enough to matter" >"$PLAY/store-local/big.bin"
run_armed "rm $PLAY/store-local/big.bin"
sess=$(ls -1d "$UNDO_DATA_DIR"/sessions/* | tail -1)
awk -F'\t' '$1=="unlink" && $4=="link"' "$sess/journal" | grep -q . ||
    fail "the unlink record does not name link as its save method"
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | tail -1)
case $bak in
    */.undo/*) ;;
    *) fail "backup landed at $bak, not in a .undo store" ;;
esac
[[ -f $bak ]] || fail "the backup file is missing"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/store-local/big.bin") == "large enough to matter" ]] ||
    fail "restore from a filesystem-local store failed"

echo "== case 26: our own store never makes an rmdir fail"
mkdir -p "$PLAY/relocate/inner"
echo "recoverable" >"$PLAY/relocate/inner/doomed.txt"
run_armed "rm $PLAY/relocate/inner/doomed.txt && rmdir $PLAY/relocate/inner"
[[ ! -e $PLAY/relocate/inner ]] ||
    fail "rmdir left the directory behind; the shim broke the command"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/relocate/inner/doomed.txt") == "recoverable" ]] ||
    fail "the backup did not survive the store evacuation"

echo "== case 27: rm -rf over the store still undoes"
mkdir -p "$PLAY/wipe/sub"
echo "keep me" >"$PLAY/wipe/sub/a.txt"
echo "me too" >"$PLAY/wipe/b.txt"
run_armed "rm -rf $PLAY/wipe"
[[ ! -e $PLAY/wipe ]] || fail "rm -rf did not run"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/wipe/sub/a.txt") == "keep me" ]] ||
    fail "a backup inside the destroyed store was not evacuated"
[[ $(cat "$PLAY/wipe/b.txt") == "me too" ]] ||
    fail "a backup inside the destroyed store was not evacuated"

echo "== case 28: a genuinely failed rmdir still reports ENOTEMPTY"
# The evacuation retry calls dir_holds_only -> opendir/readdir/closedir before
# deciding not to act, and those overwrite errno. A caller whose rmdir really
# failed must still see why, so the shim captures errno across the whole window.
mkdir -p "$PLAY/errno/sub"
touch "$PLAY/errno/keep.txt"
cat >"$WORK/errnoprobe.c" <<'CEOF'
#include <errno.h>
#include <stdio.h>
#include <unistd.h>
int main(int c, char **v)
{
    (void)c;
    errno = 0;
    if (rmdir(v[1]) == 0) {
        printf("UNEXPECTED-SUCCESS\n");
        return 1;
    }
    printf("%s\n", errno == ENOTEMPTY ? "ENOTEMPTY" : "CLOBBERED");
    return 0;
}
CEOF
if cc -o "$WORK/errnoprobe" "$WORK/errnoprobe.c" 2>/dev/null; then
    got=$(run_armed "$WORK/errnoprobe $PLAY/errno")
    [[ $got == ENOTEMPTY ]] || fail "errno after a failed rmdir reported as $got"
else
    echo "   (no cc, skipped)"
fi

echo "== case 29: a rename whose target could not be saved is recorded as lost"
# Two conditions must hold for save_file to fail: the link() attempt must cross
# filesystems (so it gets EXDEV) and the copy fallback must hit the
# UNDO_MAX_BYTES cap.  We use /dev/shm (tmpfs) for the renamed files, and we
# sabotage the store that resolve_store_root would create there by planting a
# regular file at /dev/shm/.undo, which makes ensure_store fail and forces
# save_file to use the overlay-backed session data dir -- cross-device link.
RAM=/dev/shm/undo-case29
rm -rf "$RAM"
mkdir -p "$RAM/ren"
echo "victim content" >"$RAM/ren/target.txt"
echo "source" >"$RAM/ren/src.txt"
touch /dev/shm/.undo
id=$(date +%s%N | cut -c1-16); sess="$UNDO_DATA_DIR/sessions/$id"
mkdir -p "$sess/data"; echo "mv over target" >"$sess/cmd"
env UNDO_SESSION="$sess" UNDO_MAX_BYTES=4 LD_PRELOAD="$LIB" \
    bash -c "mv $RAM/ren/src.txt $RAM/ren/target.txt"
sleep 0.01
rm -f /dev/shm/.undo
grep -q '^rename' "$sess/journal" ||
    fail "no rename record, journal: $(cat "$sess/journal")"
awk -F'\t' '$1=="rename"{print $5}' "$sess/journal" | grep -qx lost ||
    fail "rename method = '$(awk -F'\t' '$1=="rename"{print $5}' "$sess/journal")', want lost"

echo "== case 30: files the shim could not save are reported, not just recorded"
mkdir -p "$PLAY/cap"
echo original >"$PLAY/cap/big.bin"
# A cap below the file size makes the shim record a lost entry instead of a
# backup, which is what happens to a large in-place overwrite on a filesystem
# with no reflink support.
id=$(date +%s%N | cut -c1-16); sess="$UNDO_DATA_DIR/sessions/$id"
mkdir -p "$sess/data"; echo "overwrite big.bin" >"$sess/cmd"
env UNDO_SESSION="$sess" UNDO_MAX_BYTES=4 LD_PRELOAD="$LIB" \
    bash -c "echo replaced > $PLAY/cap/big.bin"
sleep 0.01
grep -q "^lost" "$sess/journal" ||
    fail "expected a lost record, journal: $(cat "$sess/journal")"

out=$("$UNDO" list)
grep -q "unprotected" <<<"$out" || fail "undo list did not flag the session: $out"

out=$("$UNDO" -n 2>&1)
grep -q "cannot be restored" <<<"$out" || fail "the preview did not warn: $out"

echo "== case 31: destroying another session's store is recorded, not silent"
mkdir -p "$PLAY/shared/work"
echo "first" >"$PLAY/shared/work/one.txt"

# session A deletes a file, which creates a store somewhere above it
a=$(date +%s%N | cut -c1-16); adir="$UNDO_DATA_DIR/sessions/$a"
mkdir -p "$adir/data"; echo "rm one" >"$adir/cmd"
env UNDO_SESSION="$adir" LD_PRELOAD="$LIB" bash -c "rm $PLAY/shared/work/one.txt"
sleep 0.01
abak=$(awk -F'\t' '$1=="unlink"{print $3}' "$adir/journal" | tail -1)
[[ -f $abak ]] || fail "session A took no backup: $(cat "$adir/journal")"

# session B destroys that store while session A is already finished
store_root=${abak%/.undo/*}
b=$(date +%s%N | cut -c1-16); bdir="$UNDO_DATA_DIR/sessions/$b"
mkdir -p "$bdir/data"; echo "rm -rf the store" >"$bdir/cmd"
env UNDO_SESSION="$bdir" LD_PRELOAD="$LIB" bash -c "rm -rf $store_root/.undo"
sleep 0.01

[[ ! -e $abak ]] || fail "the backup survived; this case proves nothing"
grep -q "^storemv" "$adir/journal" ||
    fail "session A was not told its backup is gone: $(cat "$adir/journal")"

out=$("$UNDO" list)
grep -q "unprotected" <<<"$out" || fail "undo list did not flag session A: $out"

# A dot component must not let the derived journal path escape. abs_path does
# not normalize, so without valid_session_id the id here is ".." and the
# journal built from it lands one level above the sessions directory.
mkdir -p "$PLAY/trav/.undo"
echo probe >"$PLAY/trav/probe.txt"
c=$(date +%s%N | cut -c1-16); cdir="$UNDO_DATA_DIR/sessions/$c"
mkdir -p "$cdir/data"; echo "traversal probe" >"$cdir/cmd"
env UNDO_SESSION="$cdir" LD_PRELOAD="$LIB" \
    bash -c "rm $PLAY/trav/.undo/../probe.txt"
[[ ! -e $UNDO_DATA_DIR/journal ]] ||
    fail "a .. component created a journal outside the sessions directory"

echo "== case 32: gc keeps a big hardlinked backup and prunes a big copied one"
mkdir -p "$PLAY/acct"
dd if=/dev/zero of="$PLAY/acct/deleted.bin" bs=1M count=4 status=none
dd if=/dev/zero of="$PLAY/acct/rewritten.bin" bs=1M count=4 status=none
# The copied session goes first, so it is the OLDER of the two. gc walks
# newest-first and keeps a running total that is not reduced when a session is
# pruned, so a big copy evicts everything older than it -- including sessions
# that cost nothing. Ordering the hardlink after the copy is what makes this
# case test the accounting rather than that eviction rule.
run_armed "echo small > $PLAY/acct/rewritten.bin"
mod_sess=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
run_armed "rm $PLAY/acct/deleted.bin"
del_sess=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
run_armed "touch $PLAY/acct/newest.txt"   # so neither of the above is newest

# a budget far below either backup's logical size: only the copy should count
UNDO_MAX_STORE=1048576 "$UNDO" gc >/dev/null
[[ -d $UNDO_DATA_DIR/sessions/$del_sess ]] ||
    fail "gc pruned a hardlinked backup, which allocates nothing"
[[ ! -d $UNDO_DATA_DIR/sessions/$mod_sess ]] ||
    fail "gc kept a copied backup that blew the byte budget"

echo "== case 33: the shell hook and the binary agree about this host"
# The hook and the binary each compose the origin themselves. If they disagree
# -- a different hostname source, or one of them emitting half an identity when
# the boot id is unreadable -- every session made by the hook reads as foreign
# on the machine that created it: pinned for the whole grace, and undo refusing
# to revert it. Found the hard way: the first version of the hooks wrote
# "<host><tab>" with an empty boot id, which the binary refuses to produce.
hook_origin=$(env UNDO_DATA_DIR="$WORK/store" UNDO_LIB="$LIB" \
    bash -c ". $ROOT/shell/undo.bash; printf %s \"\${_undo_origin-}\"")
echo scratch >"$PLAY/hostcheck.txt"
"$UNDO" run -- rm "$PLAY/hostcheck.txt" >/dev/null 2>&1
run_sess=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
bin_origin=$(cat "$UNDO_DATA_DIR/sessions/$run_sess/host" 2>/dev/null || true)
[[ -n $bin_origin ]] || fail "the binary recorded no origin"
[[ -n $hook_origin ]] || fail "the bash hook produced no origin"
[[ $hook_origin == "$bin_origin" ]] ||
    fail "hook origin [$hook_origin] != binary origin [$bin_origin]"

echo "== case 34: a command running on another node keeps its backups"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | head -1)
[[ -e $bak ]] || fail "case 34 setup: no backup was taken"
# what this session looks like from a different node of the cluster
printf 'othernode\tnot-our-boot-id\tpid:[999]\n' >"$sess/host"
echo 2147483647 >"$sess/pid"
rm -f "$sess/done"
for i in 1 2 3 4 5; do
    echo "filler $i" >"$PLAY/filler-$i.txt"
    run_armed "rm $PLAY/filler-$i.txt"
done
UNDO_KEEP=2 "$UNDO" gc >/dev/null
[[ -d $sess ]] || fail "gc deleted a session still running on another node"
[[ -e $bak ]] || fail "gc deleted the backup of a command still running elsewhere"

echo "== case 35: a foreign session past its grace is collectible again"
# Built with an explicitly old id rather than by aging a fresh session: the
# grace has a 15-minute floor, so "wait for it to expire" is not a test.
old=$(( $(date +%s) - 3600 ))000000
oldsess=$UNDO_DATA_DIR/sessions/$old
mkdir -p "$oldsess/data"
echo "a command that died on another node" >"$oldsess/cmd"
printf 'othernode\tnot-our-boot-id\tpid:[999]\n' >"$oldsess/host"
echo 2147483647 >"$oldsess/pid"
oldstore=$PLAY/.undo/$old
mkdir -p "$oldstore"
echo "backed up" >"$oldstore/1-1"
printf 'unlink\t%s\t%s\tlink\n' "$PLAY/oldfile.txt" "$oldstore/1-1" >"$oldsess/journal"
UNDO_KEEP=2 UNDO_FOREIGN_GRACE=1800 "$UNDO" gc >/dev/null
[[ ! -d $oldsess ]] || fail "a foreign session past its grace must be collectible"
[[ ! -e $oldstore/1-1 ]] || fail "its backup should have been reclaimed too"

echo
echo "== case 36: every record carries an integrity field the reader accepts"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
[[ -f $sess/journalv ]] || fail "case 36: no journalv beside the journal"
while IFS= read -r line; do
    [[ $line == *$'\t'~* ]] || fail "case 36: record has no integrity field: $line"
done <"$sess/journal"
# the whole point: a stamped journal still restores
"$UNDO" -y
[[ -f $PLAY/top.txt ]] || fail "case 36: a stamped journal did not restore"

echo "== case 37: a merged record is refused, not restored onto the wrong path"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
# what a short write leaves behind: a record with no newline, then the next one
{ printf 'unlink\t%s/docs/report.txt' "$PLAY"; cat "$sess/journal"; } >"$sess/j2"
mv "$sess/j2" "$sess/journal"
rm -f "$PLAY/top.txt"
"$UNDO" -y >/dev/null 2>&1 || true
[[ $(cat "$PLAY/docs/report.txt") == "report v1" ]] ||
    fail "case 37: a merged record restored a backup over the wrong path"

echo "== case 38: upgrading the shim under a session in flight keeps it restorable"
# A session an older shim already wrote unstamped records into. Declaring the
# integrity contract over those would mark every one of them corrupt, and the
# session would stop being restorable the moment the shim was upgraded.
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "a session started before the shim was upgraded" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
cp "$PLAY/top.txt" "$sess/data/legacy-1"
rm -f "$PLAY/top.txt"
printf 'unlink\t%s\t%s\tlink\n' "$PLAY/top.txt" "$sess/data/legacy-1" >"$sess/journal"
# now the new shim appends to that same journal
env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
    LD_PRELOAD="$LIB" bash -c "rm $PLAY/docs/report.txt"
[[ ! -f $sess/journalv ]] ||
    fail "case 38: the integrity contract was declared over records written \
before it existed; every one of them now reads as corrupt"
"$UNDO" apply "$id" -y >/dev/null
[[ -f $PLAY/top.txt ]] ||
    fail "case 38: a record written by the older shim was not restored"

echo "== case 39: a process that detached ignores the session it inherited"
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "the command that started the daemon" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
# setsid puts the command in a new terminal session, which is what a
# multiplexer server or any daemonizing program does
env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
    LD_PRELOAD="$LIB" setsid bash -c "rm $PLAY/top.txt" || true
[[ ! -e $PLAY/top.txt ]] || fail "case 39: the rm did not run"
[[ ! -s $sess/journal ]] ||
    fail "case 39: a detached process wrote into the session it inherited; \
after gc collects that session those writes go to an unlinked inode"

echo "== case 40: without UNDO_SID the inherited session is still honoured"
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "an older hook that does not set UNDO_SID" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" setsid bash -c "rm $PLAY/top.txt" || true
[[ -s $sess/journal ]] ||
    fail "case 40: the detach test disarmed a shell whose hook predates \
UNDO_SID; a rollout would silently stop recording"

echo
echo "all cases passed"
