#!/usr/bin/env bash
# Build two filesystems with distinct st_dev, so store-placement and EXDEV
# behavior can be tested without a cluster.
#
#   test/in-container.sh --privileged test/multifs.sh
#   test/in-container.sh --privileged test/multifs.sh path/to/assertions.sh
#
# tmpfs is used rather than loopback images: each mount gets its own st_dev,
# supports hardlinks, and needs no mkfs. A reflink-capable image is only
# needed once FICLONE is implemented, and is not built here.
set -euo pipefail

FS_A=${FS_A:-/mnt/undo-fs-a}
FS_B=${FS_B:-/mnt/undo-fs-b}
export FS_A FS_B

fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$FS_A" "$FS_B"
mount -t tmpfs -o size=64m tmpfs_a "$FS_A" ||
    fail "cannot mount tmpfs (needs --privileged)"
trap 'umount "$FS_A" 2>/dev/null; umount "$FS_B" 2>/dev/null' EXIT
mount -t tmpfs -o size=64m tmpfs_b "$FS_B" || fail "cannot mount second tmpfs"

dev_a=$(stat -c %d "$FS_A")
dev_b=$(stat -c %d "$FS_B")
[[ $dev_a != "$dev_b" ]] || fail "both mounts report st_dev=$dev_a; not distinct"
echo "== two filesystems: $FS_A (dev $dev_a), $FS_B (dev $dev_b)"

# The two properties every later placement test depends on.
echo hello >"$FS_A/src.txt"
ln "$FS_A/src.txt" "$FS_A/link.txt" || fail "hardlink within a filesystem failed"
[[ $(stat -c %h "$FS_A/src.txt") == 2 ]] || fail "link count did not rise"
echo "== hardlink within one filesystem: ok"

if ln "$FS_A/src.txt" "$FS_B/nope.txt" 2>/dev/null; then
    fail "cross-filesystem hardlink succeeded; the mounts are not distinct"
fi
echo "== cross-filesystem hardlink refused (EXDEV): ok"

# A hardlinked backup loses its extra link as soon as the original is
# removed, which is why save method cannot be recovered from st_nlink later.
rm -f "$FS_A/src.txt"
[[ $(stat -c %h "$FS_A/link.txt") == 1 ]] ||
    fail "expected nlink to fall to 1 after the original was unlinked"
echo "== nlink falls to 1 after the original is unlinked: ok"

if [[ $# -gt 0 ]]; then
    echo "== running $1"
    bash "$1"
fi

echo
echo "multifs harness ok"
