#!/usr/bin/env bash
# Smoke test for the zsh hook: source it in an isolated interactive zsh,
# delete a file, and check a session was recorded and is undoable.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/zdot" "$WORK/zdot2" "$WORK/store" "$WORK/play"
echo "precious data" >"$WORK/play/file.txt"

cat >"$WORK/zdot/.zshrc" <<EOF
export UNDO_DATA_DIR=$WORK/store
export UNDO_LIB=$ROOT/build/libundo.so
path=($ROOT/bin \$path)
source $ROOT/shell/undo.zsh
EOF

print_cmds() {
    printf 'rm %s/play/file.txt\nundo -y\ncat %s/play/file.txt\nexit\n' "$WORK" "$WORK"
}

out=$(print_cmds | ZDOTDIR=$WORK/zdot zsh -i 2>/dev/null)
if ! grep -q "precious data" <<<"$out"; then
    echo "FAIL: file not restored through the zsh hook" >&2
    echo "$out" >&2
    exit 1
fi
# A session inherited across setsid must not survive sourcing the hook.
#
# Recorded from inside .zshrc rather than asked for afterwards: _undo_preexec
# creates a fresh session before the first command runs, so an interactive
# `echo $UNDO_SESSION` always reports that new session and can never see the
# window this guards -- which is precisely the startup window in which a
# multiplexer pane would journal into a dead session.
cat >"$WORK/zdot2/.zshrc" <<EOF
export UNDO_DATA_DIR=$WORK/store
export UNDO_LIB=$ROOT/build/libundo.so
export UNDO_SESSION=$WORK/stale-session
export UNDO_SID=999999
path=($ROOT/bin \$path)
source $ROOT/shell/undo.zsh
print -r -- "AFTER_SOURCE=[\${UNDO_SESSION-unset}]" >"$WORK/observed"
EOF
printf 'exit\n' | ZDOTDIR=$WORK/zdot2 zsh -i >/dev/null 2>&1 || true
if ! grep -q 'AFTER_SOURCE=\[unset\]' "$WORK/observed" 2>/dev/null; then
    echo "FAIL: the hook kept a session inherited across setsid" >&2
    cat "$WORK/observed" 2>/dev/null >&2
    exit 1
fi
echo "stale-session guard passed"

echo "zsh hook smoke test passed"
