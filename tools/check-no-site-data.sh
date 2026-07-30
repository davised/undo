#!/usr/bin/env bash
# Refuse content carrying site-identifying data.
#
# This repository is public. The patterns here are deliberately STRUCTURAL --
# they match shapes of identifier, never specific names -- because a list of
# the exact strings being protected cannot itself live in a public repo.
#
# Point UNDO_SITE_DENYLIST at a file outside this repository (one extended
# regex per line, '#' comments allowed) to add precise patterns locally.
#
# Patterns are consumed by `grep -E`: POSIX ERE only. No lookahead, no \d, no
# non-greedy quantifiers. A pattern that fails to compile makes grep error,
# and the scanner would then pass everything -- failing open on the one check
# whose whole purpose is to fail closed. tools/check-ere.sh asserts this.
set -uo pipefail

patterns=(
    '[A-Za-z0-9-]+\.(edu|internal|local|lan)\b'   # institutional hostnames
    # RFC1918 addresses. All four octets are required: a three-component
    # pattern also matches semver, and "version": "10.0.0" in any lockfile
    # would then fire on every push. A hook that cries wolf gets bypassed.
    '\b(10\.[0-9]{1,3}|192\.168|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}\b'
    '/nfs[0-9]+/'                                 # numbered NFS mounts
    '/(fs|zfs)[0-9]+/'                            # numbered storage exports
    '\b[a-z]+[0-9]+\.[a-z0-9.-]+\.(edu|com|org)\b' # numbered server FQDNs
    # macOS home directories. Unambiguous: undo is Linux-only, so a /Users/
    # path is always someone's workstation and never documentation. /home/ is
    # deliberately NOT matched -- upstream's own README uses /home/you as a
    # placeholder, so that pattern would fire constantly and get bypassed.
    '/Users/[a-z][a-z0-9_.-]*'
)

# Expose the pattern list so tools/check-ere.sh can verify each one compiles
# under POSIX ERE, without parsing this file's source.
if [[ ${1-} == --print-patterns ]]; then
    printf '%s\n' "${patterns[@]}"
    exit 0
fi

# Exemptions live in one auditable file rather than as markers scattered
# through the tree: with a central list you can see everything that is
# excluded at a glance, which matters more for a security check than
# convenience does. One glob per line, '#' comments allowed.
#
# Reserved for documents that must quote fixtures verbatim -- notably
# anything describing this scanner. Exempting a file is in the same category
# as --no-verify and should stay rare and reviewed.
EXEMPT_FILE=${UNDO_SITE_EXEMPT:-.check-no-site-data-ignore}

is_exempt() { # is_exempt <path>
    local path=$1 pat
    [[ -r $EXEMPT_FILE ]] || return 1
    while IFS= read -r pat; do
        [[ -z $pat || $pat == \#* ]] && continue
        # shellcheck disable=SC2053  -- glob match is intended
        [[ $path == $pat || ${path#./} == $pat ]] && return 0
    done <"$EXEMPT_FILE"
    return 1
}

if [[ -n ${UNDO_SITE_DENYLIST-} && -r ${UNDO_SITE_DENYLIST-} ]]; then
    while IFS= read -r line; do
        [[ -z $line || $line == \#* ]] && continue
        patterns+=("$line")
    done <"$UNDO_SITE_DENYLIST"
fi

status=0
for f in "$@"; do
    [[ -f $f ]] || continue
    # This file only. Its own pattern list is written so that it does not
    # self-match, but the exemption stays as a safety net for future edits.
    # The TEST file is deliberately NOT exempt: it assembles fixtures from
    # inert fragments so it stays scannable, because an unscannable test file
    # is where a real identifier hides.
    case $f in tools/check-no-site-data.sh|*/tools/check-no-site-data.sh) continue ;; esac
    is_exempt "$f" && continue
    for p in "${patterns[@]}"; do
        if out=$(grep -nEI "$p" "$f" 2>/dev/null); then
            while IFS= read -r hit; do
                echo "$f:$hit" >&2
                echo "  matched: $p" >&2
            done <<<"$out"
            status=1
        fi
    done
done

if [[ $status -ne 0 ]]; then
    cat >&2 <<'MSG'

Refusing: this content looks site-identifying, and this repository is public.
Move it to the private deployment repository, or generalize it -- describe the
class of filesystem rather than the name of one.
MSG
fi
exit $status
