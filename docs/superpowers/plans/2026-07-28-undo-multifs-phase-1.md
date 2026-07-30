# undo multi-filesystem, Phase 1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the upstream `rmdir` reentrancy-guard leak and land the two pieces of infrastructure the later phases depend on — a multi-filesystem test harness and a secret-scanning hook — before any site-motivated code exists to leak.

**Architecture:** Work happens in a personal fork of upstream. The `rmdir` fix lives on its own branch cut from `upstream/main` so the pull request contains nothing else; everything fork-local lands on `hpc/main`. The test harness creates two `tmpfs` mounts to obtain distinct `st_dev` values without root-owned disk images, and runs inside a privileged container so it works from a macOS workstation.

**Tech Stack:** C (the `LD_PRELOAD` shim), Go 1.24 (the CLI), bash (test suites and hooks), podman or docker (Linux test environment), GitHub Actions (CI).

## Global Constraints

Copied verbatim from `docs/design/undo-multifs-design.md`. Every task inherits these.

- **The shim must never cause the user's command to fail.** All internal errors are swallowed; the real syscall's result is returned untouched.
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`. CI must assert the floor.
- **No site-specific data in this repository** — no hostnames, mount points, organization domains, internal addresses, usernames, or storage capacities.
- Upstream `CONTRIBUTING.md`: "Anything touching the shim (`shim/undo_shim.c`) should come with an e2e case. The shim runs inside every process a user launches: no output on the happy path, no aborts, fail open (let the real call through) when anything goes wrong."
- The build target is glibc 2.34 / x86_64. Do not introduce dependencies that require newer.

## Environment note

The development workstation is macOS; the shim is Linux-only and cannot be
built or tested natively there. Every build/test step below runs inside a
Linux container. Task 1 Step 2 creates the runner that the rest of the plan
uses.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `shim/undo_shim.c` | modify (~line 540) | the one-line guard fix |
| `test/e2e.sh` | modify (append case 21) | regression case proving the guard is cleared |
| `test/in-container.sh` | create | run any command inside a Linux container; the macOS bridge |
| `test/multifs.sh` | create | build two filesystems with distinct `st_dev`, assert link/EXDEV semantics |
| `tools/check-no-site-data.sh` | create | scan content for site identifiers |
| `.githooks/pre-push` | create | invoke the scanner before any push |
| `.github/workflows/no-site-data.yml` | create | same scan in CI, since hooks are bypassable |

---

### Task 1: Fix the `rmdir` reentrancy-guard leak

The guard `in_shim` is set on entry but cleared only when the `rmdir` both
succeeds and was journaled. After a failed `rmdir` (`ENOTEMPTY`) or one
targeting an ignored path, the flag stays set, `armed()` returns false, and the
shim silently records nothing for the remainder of that process while the
process keeps destroying files.

**Files:**
- Modify: `shim/undo_shim.c:540-556`
- Modify: `test/e2e.sh` (append after case 20, currently ends line 213)

**Interfaces:**
- Consumes: nothing (first task)
- Produces: a verified-working fork checkout, with remotes `origin` (your fork) and `upstream` (`edaywalid/undo`); branch `fix/rmdir-guard` cut from `upstream/main` holding only this fix; branch `hpc/main` holding the design docs. `test/in-container.sh` is available to later tasks.

- [ ] **Step 1: Turn the docs repo into the fork working copy**

The fork must exist on GitHub first — fork `edaywalid/undo` via the web UI or
`gh repo fork edaywalid/undo --clone=false`.

This plan refers to the two working copies by variable rather than by absolute
path, so it stays portable and carries no one's home directory:

```bash
REPO=<path to this repository>        # the public fork working copy
SITE_REPO=<path to the private repo>  # deployment config and site addendum
FORK=git@github.com:<your-user>/undo.git
```

```bash
cd "$REPO"
git remote add origin   "$FORK"
git remote add upstream https://github.com/edaywalid/undo.git
git fetch upstream --tags

# the four existing commits are docs only; replay them onto upstream's history
git branch -m main hpc/main
git rebase --onto upstream/main --root hpc/main
git log --oneline -6
```

Expected: upstream's commits below your four docs commits, and
`ls shim/undo_shim.c` now resolves.

- [ ] **Step 2: Create the container runner**

Create `test/in-container.sh`:

```bash
#!/usr/bin/env bash
# Run a command inside a Linux container against a copy of the working tree.
# The shim is Linux-only, so this is how it gets built and tested from macOS.
#
#   test/in-container.sh make test
#   test/in-container.sh --privileged test/multifs.sh
set -euo pipefail

IMAGE=${UNDO_TEST_IMAGE:-docker.io/library/golang:1.24-bookworm}
ROOT=$(cd "$(dirname "$0")/.." && pwd)

engine=""
for e in podman docker; do
    command -v "$e" >/dev/null 2>&1 && { engine=$e; break; }
done
[ -n "$engine" ] || { echo "in-container: need podman or docker" >&2; exit 1; }

opts=()
if [ "${1-}" = "--privileged" ]; then
    opts+=(--privileged)
    shift
fi
[ $# -gt 0 ] || { echo "usage: in-container.sh [--privileged] <cmd>..." >&2; exit 2; }

# The tree is mounted read-only and copied in: tests write freely without
# touching the host checkout, and a failed run leaves nothing behind.
exec "$engine" run --rm "${opts[@]}" -v "$ROOT":/src:ro -w / "$IMAGE" bash -c '
  set -e
  mkdir -p /w && cp -r /src/. /w/ && cd /w
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null 2>&1
  apt-get install -y -qq gcc make >/dev/null 2>&1
  '"$(printf '%q ' "$@")"'
'
```

```bash
chmod +x test/in-container.sh
```

- [ ] **Step 3: Establish the baseline**

Run: `test/in-container.sh make test`

Expected: builds, then `all cases passed`. If this fails, stop — the
environment is wrong and nothing below will be meaningful.

- [ ] **Step 4: Cut the PR branch from upstream**

The pull request must contain only this fix, so it is cut from upstream's
history rather than from the docs branch.

```bash
git checkout -b fix/rmdir-guard upstream/main
```

- [ ] **Step 5: Write the failing regression case**

Append to `test/e2e.sh`, after case 20 and before the final `echo` /
`echo "all cases passed"` lines:

```bash
echo "== case 21: a failed rmdir does not silence the rest of the process"
# The shim uses a thread-local flag to avoid re-entering itself. rmdir used to
# clear that flag only when the rmdir succeeded and was journaled, so a single
# ENOTEMPTY left the shim disabled for the remainder of the process and every
# later change went unrecorded while still happening on disk.
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
    "$UNDO" -y >/dev/null
    [[ $(cat "$PLAY/guard/keep.txt") == precious ]] ||
        fail "an unlink after a failed rmdir was not captured"
else
    echo "   (no cc, skipped)"
fi
```

- [ ] **Step 6: Run it and watch it fail**

Run: `test/in-container.sh test/e2e.sh`

Expected: `FAIL: an unlink after a failed rmdir was not captured`. If it
passes, the test is not exercising the bug — check that `$PLAY/guard`
is genuinely non-empty when `rmdir` runs.

- [ ] **Step 7: Apply the fix**

In `shim/undo_shim.c`, replace the body of `rmdir()` (lines 540-556):

```c
int rmdir(const char *path)
{
    REAL(rmdir, int, const char *);
    if (!armed())
        return real_rmdir(path);
    in_shim = 1;
    char abs[PATH_MAX], mode[8];
    int ok;
    handle_rmdir_pre(AT_FDCWD, path, abs, mode, &ok);
    int rc = real_rmdir(path);
    if (rc == 0 && ok)
        jwrite("rmdir", abs, mode, NULL);
    in_shim = 0;
    return rc;
}
```

Two changes: the redundant inner `in_shim = 1` / `in_shim = 0` around `jwrite`
is dropped (the flag is already set), and the reset now happens on every path.

- [ ] **Step 8: Run the full suite**

Run: `test/in-container.sh make test`

Expected: all Go tests pass, then `all cases passed` including case 21.

- [ ] **Step 9: Confirm the glibc floor did not move**

Run:

```bash
test/in-container.sh bash -c '
  gcc -shared -fPIC -O2 -Wall -o /tmp/libundo.so shim/undo_shim.c -ldl
  objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
```

Expected: `GLIBC_2.34` or lower. Anything higher must be resolved before the
change can ship.

- [ ] **Step 10: Commit the fix**

```bash
git add shim/undo_shim.c test/e2e.sh
git commit -m 'shim: always clear the reentrancy guard in rmdir

rmdir set in_shim on entry but cleared it only when the rmdir both
succeeded and was journaled. After an ENOTEMPTY, or an rmdir of a path on
the ignore list, the flag stayed set for the rest of the process: armed()
returned false and every subsequent change went unrecorded while still
happening on disk.

A program that calls a failing rmdir() and then unlink() left an empty
journal and an unrecoverable file. Callers going through
unlinkat(AT_REMOVEDIR) were unaffected, which is why coreutils rm -rf
never showed it.'
```

- [ ] **Step 11: Push the branch and open the pull request**

```bash
git push -u origin fix/rmdir-guard
```

Open the PR against `edaywalid/undo` `main`. Confirm the diff shows exactly
two files and no documentation.

- [ ] **Step 12: Return to the fork branch**

```bash
git checkout hpc/main
git merge --no-ff fix/rmdir-guard -m 'merge the rmdir guard fix'
```

---

### Task 2: Multi-filesystem test harness

Later phases place backups on the same filesystem as the file being saved.
Testing that needs two filesystems. `tmpfs` mounts each carry their own
`st_dev`, so no disk images or `mkfs` are required — but mounting needs
`CAP_SYS_ADMIN`, hence `--privileged`.

**Files:**
- Create: `test/multifs.sh`

**Interfaces:**
- Consumes: `test/in-container.sh` from Task 1.
- Produces: `test/multifs.sh`, which exports `FS_A` and `FS_B` (paths on distinct filesystems) and runs `$1` as an assertion script if given. Phase 2 tests will source it.

- [ ] **Step 1: Write the harness with its own self-check**

Create `test/multifs.sh`:

```bash
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
```

```bash
chmod +x test/multifs.sh
```

- [ ] **Step 2: Run it**

Run: `test/in-container.sh --privileged test/multifs.sh`

Expected:

```
== two filesystems: /mnt/undo-fs-a (dev N), /mnt/undo-fs-b (dev M)
== hardlink within one filesystem: ok
== cross-filesystem hardlink refused (EXDEV): ok
== nlink falls to 1 after the original is unlinked: ok

multifs harness ok
```

The third assertion is the one that matters most: it is the executable proof
of the finding that removed `st_nlink` accounting from the design.

- [ ] **Step 3: Verify it fails honestly without privileges**

Run: `test/in-container.sh test/multifs.sh`

Expected: `FAIL: cannot mount tmpfs (needs --privileged)`, exit non-zero. A
harness that silently degrades to one filesystem would make later placement
tests pass for the wrong reason.

- [ ] **Step 4: Commit**

```bash
git add test/multifs.sh test/in-container.sh
git commit -m 'test: multi-filesystem harness and container runner

Two tmpfs mounts give distinct st_dev without disk images, which is what
store-placement and EXDEV tests need. Asserts hardlink-within, EXDEV-across,
and that nlink falls back to 1 once the original is unlinked -- the property
that makes save method unrecoverable after the fact.

in-container.sh builds and tests the Linux-only shim from a macOS checkout.'
```

---

### Task 3: Secret-scanning hook and CI job

This repository is public and will receive site-motivated changes. The scanner
must exist before there is anything to leak.

The denylist itself is the problem: a list of the exact strings to protect
cannot live in the public repository. So the public scanner carries only
**structural** patterns that catch classes of leak without naming anything, and
optionally reads a private supplement from outside the repository.

**Files:**
- Create: `tools/check-no-site-data.sh`
- Create: `.githooks/pre-push`
- Create: `.github/workflows/no-site-data.yml`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `tools/check-no-site-data.sh <file>...` — exits 0 when clean, 1 when a match is found, printing `file:line: pattern` for each. Reads extra patterns, one regex per line, from `$UNDO_SITE_DENYLIST` if that variable points at a readable file.

- [ ] **Step 1: Write the failing test for the scanner**

Create `tools/check-no-site-data.test.sh`:

```bash
#!/usr/bin/env bash
# Tests for the site-data scanner.
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

# Fixtures are ASSEMBLED FROM FRAGMENTS that individually match nothing, so
# this file stays scannable and needs no exemption. An exempt test file is
# unscannable by definition, which is exactly where a real identifier hides.
EDU_HOST="host.dept.example"".edu"
NFS_PATH="/nfs0""/example/scratch"
MAC_PATH="/Users""/someone/projects"
IP_10="10.99.""99.99"

check "clean prose passes"            0 'The store is resolved at runtime.'
check "an institutional host caught"  1 "measured on $EDU_HOST"
check "a numbered nfs mount caught"   1 "files under $NFS_PATH"
check "a workstation path caught"     1 "run it from $MAC_PATH"
check "10/8 caught"                   1 "mon host at $IP_10:6789"
check "semver is not an IP"           0 '"version": "10.0.0", "node": "^10.6.0"'
check "upstream /home/you is fine"    0 'echo /home/you/secrets >> ignore'
check "a generic path is fine"        0 'files under /net/volume/user'

echo "$pass passed, $failed failed"
[[ $failed -eq 0 ]]
```

The fragment trick is the important part. Writing fixtures literally forces
the test file to be exempted from scanning, and an exempt file is where a real
identifier survives review. Splitting each fixture so neither half matches
keeps the file inside the scanner's coverage.

```bash
chmod +x tools/check-no-site-data.test.sh
```

- [ ] **Step 2: Run it and watch it fail**

Run: `tools/check-no-site-data.test.sh`

Expected: fails immediately — `check-no-site-data.sh` does not exist yet.

- [ ] **Step 3: Write the scanner**

Create `tools/check-no-site-data.sh`:

```bash
#!/usr/bin/env bash
# Refuse content carrying site-identifying data.
#
# This repository is public. The patterns here are deliberately STRUCTURAL --
# they match shapes of identifier, never specific names -- because a list of
# the exact strings being protected cannot itself live in a public repo.
#
# Point UNDO_SITE_DENYLIST at a file outside this repository (one extended
# regex per line, '#' comments allowed) to add precise patterns locally.
set -uo pipefail

patterns=(
    '[A-Za-z0-9-]+\.(edu|internal|local|lan)\b'   # institutional hostnames
    '\b(10|192\.168)\.[0-9]{1,3}\.[0-9]{1,3}\b'   # RFC1918 addresses
    '\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\b'  # RFC1918, 172.16/12
    '/nfs[0-9]+/'                                 # numbered NFS mounts
    '/(fs|zfs)[0-9]+/'                            # numbered storage exports
    '\b[a-z]+[0-9]+\.[a-z0-9.-]+\.(edu|com|org)\b' # numbered server FQDNs
    # macOS home directories. Unambiguous: undo is Linux-only, so a /Users/
    # path is always someone's workstation and never documentation. /home/ is
    # deliberately NOT matched -- upstream's own README uses /home/you as a
    # placeholder, so it would fire constantly and get bypassed.
    '/Users/[a-z][a-z0-9_.-]*'
)

if [[ -n ${UNDO_SITE_DENYLIST-} && -r ${UNDO_SITE_DENYLIST-} ]]; then
    while IFS= read -r line; do
        [[ -z $line || $line == \#* ]] && continue
        patterns+=("$line")
    done <"$UNDO_SITE_DENYLIST"
fi

status=0
for f in "$@"; do
    [[ -f $f ]] || continue
    case $f in tools/check-no-site-data.*) continue ;; esac  # self-exempt
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
```

```bash
chmod +x tools/check-no-site-data.sh
```

The scanner exempts its own test and source files, which necessarily contain
sample patterns.

- [ ] **Step 4: Run the tests**

Run: `tools/check-no-site-data.test.sh`

Expected: `7 passed, 0 failed`, exit 0.

- [ ] **Step 5: Verify it catches the real thing**

The private addendum is a known-positive control. Run:

```bash
tools/check-no-site-data.sh "$SITE_REPO/docs/site-addendum.md"; echo "exit=$?"
```

Expected: many matches, `exit=1`. Then confirm no false positive on this
repository's own documentation:

```bash
tools/check-no-site-data.sh docs/design/undo-multifs-design.md; echo "exit=$?"
```

Expected: no output, `exit=0`.

- [ ] **Step 6: Add the pre-push hook**

Create `.githooks/pre-push`:

```bash
#!/usr/bin/env bash
# Scan every file changed against the upstream tip before allowing a push.
set -uo pipefail
root=$(git rev-parse --show-toplevel)
base=$(git merge-base HEAD origin/main 2>/dev/null || echo HEAD~1)
mapfile -t files < <(git diff --name-only --diff-filter=ACMR "$base" HEAD)
[[ ${#files[@]} -eq 0 ]] && exit 0
cd "$root" && exec tools/check-no-site-data.sh "${files[@]}"
```

```bash
chmod +x .githooks/pre-push
git config core.hooksPath .githooks
```

Note in the commit message that `core.hooksPath` is per-clone and must be set
again after any fresh clone — this is why Step 7 exists.

- [ ] **Step 7: Add the CI job**

Hooks are local and bypassable with `--no-verify`, so the same check runs in
CI. Create `.github/workflows/no-site-data.yml`:

```yaml
name: no-site-data

on:
  push:
    branches: [main, 'hpc/**']
  pull_request:

jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - name: scanner self-test
        run: tools/check-no-site-data.test.sh
      - name: scan tracked files
        run: git ls-files -z | xargs -0 tools/check-no-site-data.sh
```

- [ ] **Step 8: Prove the hook actually blocks**

```bash
# assembled so this plan itself stays scannable
printf 'measured on %s\n' "host.dept.example"".edu" > ./leak-test.md
git add leak-test.md && git commit -q -m 'temp: leak test'
git push --dry-run origin hpc/main; echo "exit=$?"
```

Expected: the hook prints the match and `exit=1`, refusing the push. Then
clean up:

```bash
git reset --hard HEAD~1 && rm -f ./leak-test.md /tmp/leak.md
```

- [ ] **Step 9: Confirm no ERE portability problem**

The patterns are consumed by `grep -E`, which is POSIX ERE and has **no**
lookahead, lookbehind, `\d`, or non-greedy quantifiers. Confirm every pattern
compiles under plain ERE before trusting the scanner:

```bash
bash -c 'source /dev/stdin <<<"$(sed -n "/^patterns=(/,/^)/p" tools/check-no-site-data.sh)"
for p in "${patterns[@]}"; do
  echo x | grep -qE "$p" 2>/dev/null || [ $? -eq 1 ] || echo "BAD PATTERN: $p"
done; echo "pattern syntax ok"'
```

Expected: `pattern syntax ok` with no `BAD PATTERN` lines. A pattern that
fails to compile makes `grep` error out, and the scanner would then pass
everything — failing open on the one check whose whole purpose is to fail
closed.

- [ ] **Step 10: Confirm the whole tree is clean, then commit**

```bash
git ls-files -z | xargs -0 tools/check-no-site-data.sh; echo "exit=$?"
```

Expected: `exit=0`.

```bash
git add tools/check-no-site-data.sh tools/check-no-site-data.test.sh \
        .githooks/pre-push .github/workflows/no-site-data.yml
git commit -m 'tools: refuse site-identifying content on push and in CI

This repo is public and will take site-motivated changes, so the scanner
lands before there is anything to leak.

Patterns are structural rather than literal -- institutional hostnames,
RFC1918 addresses, numbered storage mounts -- because a list of the exact
strings being protected cannot live in a public repository. Set
UNDO_SITE_DENYLIST to a file outside the repo to add precise patterns.

The hook needs "git config core.hooksPath .githooks" in every fresh clone,
and is bypassable with --no-verify, which is why CI runs the same check.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes, including new case 21
- [ ] Case 21 fails on `upstream/main` and passes with the fix
- [ ] The shim's glibc floor is still `GLIBC_2.34` or lower
- [ ] `fix/rmdir-guard` is pushed and a pull request is open against upstream, containing exactly two files
- [ ] `test/in-container.sh --privileged test/multifs.sh` passes all four assertions
- [ ] `test/multifs.sh` fails loudly without `--privileged`
- [ ] `tools/check-no-site-data.test.sh` reports `7 passed, 0 failed`
- [ ] Every scanner pattern compiles under POSIX ERE (`grep -E`), so the
      scanner cannot fail open
- [ ] The scanner flags the private addendum and clears this repository
- [ ] A push carrying a planted identifier is refused by the hook

## Deliberately not in this phase

- Store placement, the containment guard, and the resolver cache (phase 2)
- Converting the five `os.Rename` call sites in `restore.go` (phase 2, required for cross-device restore)
- Recording the save method in the journal, and GC accounting (phase 2)
- `FICLONE`, the copy ladder, the `lost` warning, per-volume `doctor` (phase 3)
- The agent skill (phase 4)

## Notes for the implementer

- The `rmdir` fix is two lines but the reasoning matters: the redundant inner
  `in_shim = 1` / `in_shim = 0` pair is removed because the flag is already
  set at that point. Keeping it would work, but it is what disguised the bug.
- Case 21 must use a compiled C program, not a shell command. `rmdir(1)` and
  `rm(1)` are separate processes, and `in_shim` is thread-local — the leak is
  only observable within one process. A shell-based test would pass whether or
  not the bug exists.
- Do not add the container runner to the upstream pull request. It is
  fork-local tooling; upstream CI runs on Linux natively.
