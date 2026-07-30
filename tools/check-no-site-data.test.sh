#!/usr/bin/env bash
# Tests for the site-data scanner.
#
# IMPORTANT: fixtures are ASSEMBLED FROM FRAGMENTS that individually do not
# match any pattern, so this file is itself scannable and needs no exemption.
# That is deliberate. An exempt test file is unscannable by definition, so a
# real identifier pasted into one ships silently -- which is exactly what
# happened here once already. With fragments, a real address written out
# literally gets caught like anywhere else.
set -uo pipefail
SCAN=$(cd "$(dirname "$0")" && pwd)/check-no-site-data.sh
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
pass=0; failed=0
check() { # check <desc> <expected-exit> <file-content>
    printf '%s\n' "$3" >"$TMP/sample.md"
    "$SCAN" "$TMP/sample.md" >/dev/null 2>&1
    if [[ $? -eq $2 ]]; then pass=$((pass+1))
    else echo "FAIL: $1"; failed=$((failed+1)); fi
}

# Fragments. Each half is inert; only the concatenation matches.
EDU_HOST="host.dept.example"".edu"
NFS_PATH="/nfs0""/example/scratch"
MAC_PATH="/Users""/someone/projects"
IP_10="10.99.""99.99"
IP_192="192.168.""99.99"
IP_172="172.16.""4.2"

check "clean prose passes"            0 'The store is resolved at runtime.'
check "an institutional host caught"  1 "measured on $EDU_HOST"
check "a numbered nfs mount caught"   1 "files under $NFS_PATH"
check "a workstation path caught"     1 "run it from $MAC_PATH"
check "upstream /home/you is fine"    0 'echo /home/you/secrets >> ignore'
check "a generic path is fine"        0 'files under /net/volume/user'
# Regressions found by running the scanner against the whole tree.
check "semver is not an IP"           0 '"version": "10.0.0", "node": "^10.6.0"'
check "10/8 caught"                   1 "mon host at $IP_10:6789"
check "192.168/16 caught"             1 "gateway $IP_192"
check "172.16/12 caught"              1 "gateway $IP_172"

# The exempt list is path-based. Verify it exempts what it lists and nothing
# else -- an over-broad glob would silently disable the scanner.
exempt_check() { # exempt_check <desc> <expected-exit> <relpath> <globline>
    mkdir -p "$TMP/repo/$(dirname "$3")"
    printf '%s\n' "$EDU_HOST" >"$TMP/repo/$3"
    printf '%s\n' "$4" >"$TMP/repo/.check-no-site-data-ignore"
    ( cd "$TMP/repo" && "$SCAN" "$3" >/dev/null 2>&1 )
    if [[ $? -eq $2 ]]; then pass=$((pass+1))
    else echo "FAIL: $1"; failed=$((failed+1)); fi
}
exempt_check "a listed path is exempt"     0 'docs/plans/p.md' 'docs/plans/*.md'
exempt_check "an unlisted path is scanned" 1 'docs/design/d.md' 'docs/plans/*.md'
exempt_check "no ignore file means scan"   1 'docs/plans/p.md' '# nothing'

echo "$pass passed, $failed failed"
[[ $failed -eq 0 ]]
