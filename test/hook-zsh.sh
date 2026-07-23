#!/usr/bin/env bash
# Smoke test for the zsh hook: source it in an isolated interactive zsh,
# delete a file, and check a session was recorded and is undoable.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/zdot" "$WORK/store" "$WORK/play"
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
echo "zsh hook smoke test passed"
