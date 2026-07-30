#!/usr/bin/env bash
# Assert every scanner pattern compiles under POSIX ERE.
#
# grep -E has no lookahead, no \d, and no non-greedy quantifiers. A pattern
# borrowed from PCRE makes grep exit 2 with a parse error, and the scanner's
# `if out=$(grep ...)` treats that as "no match" -- so a malformed pattern
# silently stops checking anything. That is failing open on a check whose
# entire job is to fail closed, which is why this runs in CI.
set -uo pipefail

SCAN=$(cd "$(dirname "$0")" && pwd)/check-no-site-data.sh
bad=0 n=0

while IFS= read -r p; do
    [[ -z $p ]] && continue
    n=$((n + 1))
    # grep exits 0 on match, 1 on no match, 2 on a bad pattern.
    printf 'x\n' | grep -qE "$p" 2>/dev/null
    if [[ $? -ge 2 ]]; then
        echo "BAD PATTERN (not valid POSIX ERE): $p" >&2
        bad=$((bad + 1))
    fi
done < <("$SCAN" --print-patterns)

if [[ $n -eq 0 ]]; then
    echo "no patterns found; check-no-site-data.sh --print-patterns is broken" >&2
    exit 1
fi
if [[ $bad -ne 0 ]]; then
    echo "$bad of $n patterns are invalid" >&2
    exit 1
fi
echo "$n patterns, all valid POSIX ERE"
