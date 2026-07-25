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

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    grep -m1 '"tag_name"' | cut -d'"' -f4) || fail "could not query latest release"
ver=${tag#v}
url="https://github.com/$REPO/releases/download/$tag/undo_${ver}_linux_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading undo $tag (linux/$arch)..."
curl -fsSL "$url" | tar -xz -C "$tmp" || fail "download failed: $url"

install -Dm755 "$tmp/undo" "$PREFIX/bin/undo"
install -Dm755 "$tmp/build/libundo_${arch}.so" "$PREFIX/lib/undo/libundo.so"
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
echo
echo "add the hook for your shell:"
echo "  zsh:   echo 'source $PREFIX/share/undo/undo.zsh'  >> ~/.zshrc"
echo "  bash:  echo 'source $PREFIX/share/undo/undo.bash' >> ~/.bashrc"
echo "  fish:  echo 'source $PREFIX/share/undo/undo.fish' >> ~/.config/fish/config.fish"
echo
echo "then open a new shell and try:  touch x && rm x && undo"
