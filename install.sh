#!/bin/sh
# undo installer: fetches the latest release and installs to ~/.local
# (no root needed). Usage:  curl -fsSL <url>/install.sh | sh
set -eu

REPO="edaywalid/undo"
PREFIX="${PREFIX:-$HOME/.local}"

fail() { echo "install.sh: $*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || fail "undo is Linux-only (LD_PRELOAD based)"

case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "unsupported architecture: $(uname -m). Build from source: make install" ;;
esac

if ldd --version 2>&1 | grep -qi musl; then
    echo "note: musl libc detected. The prebuilt shim targets glibc;" >&2
    fail "build from source instead: git clone https://github.com/$REPO && cd undo && make install"
fi

# fetch fully before parsing: piping straight into grep -m1 closes the pipe
# early and makes curl print a confusing "(23) Failure writing output" error
api=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest") ||
    fail "could not query latest release"
tag=$(printf '%s\n' "$api" | grep '"tag_name"' | head -n1 | cut -d'"' -f4)
[ -n "$tag" ] || fail "could not parse the latest release tag"
ver=${tag#v}
url="https://github.com/$REPO/releases/download/$tag/undo_${ver}_linux_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading undo $tag (linux/$arch)..."
curl -fsSL "$url" | tar -xz -C "$tmp" || fail "download failed: $url"

# The binary and the shim may be running or mapped right now (this is
# also how `undo upgrade` reinstalls), and writing over those fails with
# "Text file busy". Land them beside the target, then rename into place:
# rename is atomic and leaves existing processes on the old inode.
replace() { # replace <src> <dst> <mode>
    install -Dm"$3" "$1" "$2.new" || fail "could not write $2.new"
    mv -f "$2.new" "$2" || fail "could not replace $2"
}
replace "$tmp/undo" "$PREFIX/bin/undo" 755
replace "$tmp/build/libundo_${arch}.so" "$PREFIX/lib/undo/libundo.so" 755
install -Dm644 "$tmp/shell/undo.zsh" "$PREFIX/share/undo/undo.zsh"
install -Dm644 "$tmp/shell/undo.bash" "$PREFIX/share/undo/undo.bash"
install -Dm644 "$tmp/shell/undo.fish" "$PREFIX/share/undo/undo.fish"
install -Dm644 "$tmp/completions/_undo" "$PREFIX/share/zsh/site-functions/_undo"
install -Dm644 "$tmp/completions/undo.bash" "$PREFIX/share/bash-completion/completions/undo"
install -Dm644 "$tmp/completions/undo.fish" "$PREFIX/share/fish/vendor_completions.d/undo.fish"

echo
echo "installed undo $tag to $PREFIX"
case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *) echo "note: $PREFIX/bin is not on your PATH" ;;
esac

# undo does nothing until its hook is loaded, and an install that stops at
# "here is a line to paste" leaves people with a tool that silently records
# nothing. Wire it up, idempotently, unless told not to.
hook_line=""
path_line=""
rc=""
case "${SHELL##*/}" in
    zsh)  rc="$HOME/.zshrc";                  hook_line="source $PREFIX/share/undo/undo.zsh" ;;
    bash) rc="$HOME/.bashrc";                 hook_line="source $PREFIX/share/undo/undo.bash" ;;
    fish) rc="$HOME/.config/fish/config.fish"; hook_line="source $PREFIX/share/undo/undo.fish" ;;
esac

# an install that leaves `undo` unrunnable is not an install
case ":$PATH:" in
    *":$PREFIX/bin:"*) ;;
    *)
        case "${SHELL##*/}" in
            fish) path_line="fish_add_path $PREFIX/bin" ;;
            zsh | bash) path_line="export PATH=\"$PREFIX/bin:\$PATH\"" ;;
        esac
        ;;
esac

echo
if [ -n "${UNDO_NO_MODIFY_RC-}" ] || [ -z "$rc" ]; then
    echo "add the hook for your shell, then open a new terminal:"
    echo "  zsh:   echo 'source $PREFIX/share/undo/undo.zsh'  >> ~/.zshrc"
    echo "  bash:  echo 'source $PREFIX/share/undo/undo.bash' >> ~/.bashrc"
    echo "  fish:  echo 'source $PREFIX/share/undo/undo.fish' >> ~/.config/fish/config.fish"
elif [ -f "$rc" ] && grep -qF "$hook_line" "$rc"; then
    echo "hook already set up in $rc"
else
    mkdir -p "$(dirname "$rc")"
    {
        printf '\n# undo: revert what the last command did (undo.edaywalid.com)\n'
        [ -n "$path_line" ] && printf '%s\n' "$path_line"
        printf '%s\n' "$hook_line"
    } >>"$rc"
    echo "hook added to $rc"
    [ -n "$path_line" ] && echo "added $PREFIX/bin to your PATH there too"
    echo "(re-run with UNDO_NO_MODIFY_RC=1 to skip this next time)"
fi

echo
echo "open a new terminal, then try it (undo needs its own line):"
echo "  touch x"
echo "  rm x"
echo "  undo"
