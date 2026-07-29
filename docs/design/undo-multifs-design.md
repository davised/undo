# undo on multi-filesystem hosts: design

**Status:** proposed
**Date:** 2026-07-28
**Upstream:** `edaywalid/undo` (MIT)

This document is deliberately site-agnostic. It describes classes of
filesystem, never named hosts, mounts, or organizations. Site-specific
configuration, measurements, and deployment values live in a separate
private addendum. See "Repository hygiene" below.

## Summary

`undo` is an `LD_PRELOAD` shim that journals destructive libc calls so a CLI
can replay them in reverse. It assumes one store directory for all backups.
On a host with many independently mounted filesystems, that assumption fails
silently: backups degrade from free hardlinks to size-capped copies, and above
the cap nothing is saved at all.

This design makes the store filesystem-local, fixes the size accounting that
evicts large backups immediately, repairs an upstream reentrancy bug, and
makes unprotected files visible instead of silent.

## Background: what undo stores, and what it costs

Two mechanisms, with very different economics:

- **Deletions** are saved by `link(2)` into the store. A hardlink is a second
  name for existing blocks, so this allocates nothing and has no size limit.
  The cost is deferred reclamation: the blocks are not freed until the backup
  is pruned.
- **In-place overwrites** (`O_TRUNC`, `truncate`, editors writing in place)
  cannot be hardlinked, because the bytes are about to be rewritten. These are
  byte copies, capped by `UNDO_MAX_BYTES` (256 MiB default). Above the cap the
  shim writes a `lost` journal record and saves nothing.

The journal already records backup locations as absolute paths, so distributed
backups need no change to how locations are *expressed*. Two related claims
are, however, false, and were corrected after review:

- **The journal does need one additive field.** The save method (hardlink,
  reflink, or byte copy) cannot be recovered after the fact — see item 4 — so
  the shim must record it at save time. The field is appended, and
  `journal.Read` already tolerates trailing fields, so older readers degrade
  gracefully.
- **Restore does not already tolerate cross-device moves.** `moveAny` does,
  but five call sites bypass it and call `os.Rename` directly
  (`restore.go:283–285`, `:303`, `:314`), plus `swapAny` at `:90`. These fail
  `EXDEV` once the store is on a different filesystem from the target and must
  be converted. This is required work, not an inherited property.

## Failure modes on a multi-filesystem host

1. **Cross-filesystem hardlinks are impossible.** `link(2)` returns `EXDEV`
   across mounts. With a single store, every file on a different filesystem
   from the store degrades to a capped copy — so on such a host the common
   case (large file deleted on bulk storage, store in the home directory)
   silently keeps nothing.

2. **Hardlinked backups are counted at full size.** GC sums file sizes, so a
   hardlinked 50 GiB backup counts 50 GiB against a 1 GiB default budget and
   is evicted on the next command. Large deletions are therefore unprotected
   in practice even when the hardlink succeeded. This defeats the mechanism
   that is otherwise free.

3. **`rmdir()` leaks its reentrancy guard** (upstream bug). The guard
   `in_shim` is set on entry but cleared only on the success path:

   ```c
   int rc = real_rmdir(path);
   if (rc == 0 && ok) {
       in_shim = 1;
       jwrite("rmdir", abs, mode, NULL);
       in_shim = 0;          /* only reset here */
   }
   return rc;
   ```

   After a `rmdir` that fails (e.g. `ENOTEMPTY`) or targets an ignored path,
   `in_shim` stays set for the remainder of the process, `armed()` returns
   false, and the shim silently records nothing further while the process
   keeps destroying files. Confirmed by reproduction: a program that calls a
   failing `rmdir()` then `unlink()` leaves an empty journal and an
   unrecoverable file.

   Scope: reached via the plain `rmdir()` wrapper. Callers that use
   `unlinkat(AT_REMOVEDIR)` — including coreutils `rm -rf`, Python's
   `shutil.rmtree`, and `git clean` — are unaffected, because `unlinkat()`
   clears the guard correctly.

4. **Hosts are heterogeneous.** `st_dev` values are assigned at mount time and
   differ between hosts; mount sets differ too. No static, site-wide
   configuration keyed to device numbers or mount paths can be correct
   everywhere.

## Design

### 1. Fix the `rmdir` guard (upstream)

Clear `in_shim` unconditionally before returning. One line. This is a plain
bug fix with no relation to multi-filesystem support and should be submitted
upstream on its own.

### 2. Filesystem-local store placement

Replace the single store with one store per filesystem, resolved at runtime
from the file being saved.

**Algorithm.** Given absolute path `P`:

1. `d = st_dev(P)`.
2. Walk upward from `dirname(P)`, stopping when `st_dev` changes (the mount
   boundary) or the root is reached.
3. Track the **highest** ancestor that is still on `d`, is owned by the
   effective uid, and is writable.
4. **Reject the candidate if it is the operated-on path itself, or a
   descendant of it.** See "Containment guard" below.
5. That ancestor is the store root; backups go to
   `<ancestor>/.undo/<session-id>/`.
6. If no such ancestor exists, return failure and fall back to
   `$UNDO_SESSION/data` (upstream behavior).

**Containment guard.** A store root that lies inside the tree being modified
would place backups in the directory they exist to protect — a recursive
delete would then race its own safety net. This is reachable whenever the
highest owned ancestor is itself the target: `rm -rf <your top-level
directory on a volume>` resolves the store to that same directory.

Found by testing on a filesystem whose per-user directories sit under a
group-owned parent, where the walk terminated inside the working tree.

A naive prefix comparison is **not** sufficient, for three reasons:

1. `abs_path` (`undo_shim.c:122`) does no normalization — it concatenates cwd
   or the `/proc/self/fd` link with the given path. `..` components, doubled
   separators, and symlinks all survive, so `/x/tree/../tree` does not compare
   equal to `/x/tree`.
2. Prefix matching without a separator boundary confuses `/x/a` with `/x/ab`.
3. The `$UNDO_SESSION/data` fallback may itself lie beneath the operated tree,
   so falling back is not automatically safe.

The guard therefore compares on `/`-delimited component boundaries, rejects
any candidate containing unresolved `..`, and applies the same containment
test to the fallback. Full symlink resolution is deliberately **not**
attempted on every call — `realpath` on a network filesystem is expensive and
the shim is on the hot path — so aliasing through symlinks or bind mounts
remains a known, documented gap rather than a solved problem.

**Store creation must not change the operation's outcome.** Creating
`.undo/` inside a directory can make an otherwise-successful `rmdir` fail with
`ENOTEMPTY`, which violates Invariant 1. Mitigations, in order:

- Place the store as high as possible (the algorithm already maximizes
  height), so it is rarely a sibling of anything being removed.
- Create the store lazily, on the first backup that actually needs it.
- Never create it inside the operated path (the guard above).
- On `rmdir`/`unlinkat(AT_REMOVEDIR)` failure, if the target directory's only
  remaining entry is our own store, remove the store and retry once.

The last item is the only one that fully closes the hole, and it is required.

**Which path drives selection for renames.** A rename has a source, a
destination, and possibly an overwritten destination inode. The backup taken
is of the *destination* being overwritten (`undo_shim.c:483`), so the
destination path selects the store. Containment is checked against both source
and destination. `RENAME_EXCHANGE` takes no backup and needs no store.

**Why "owned by the caller" rather than merely writable.** On shared storage
the mount root is typically not user-writable, while a directory the user owns
inside the volume is. Selecting the highest *owned* ancestor lands on the
user's own top-level directory on that volume, which is both writable and a
natural place for a private store. Selecting merely the highest *writable*
ancestor risks landing in a shared directory, where one user's backups would
sit in space belonging to everyone.

**Deployment consequence.** Volumes differ in whether per-user directories are
user-owned or sit beneath a group-owned parent. Where the parent is
group-owned and the user has no directory of their own yet, the walk finds no
owned ancestor and undo degrades to capped copies in the session directory —
safe, but much less useful. Deployment must therefore pre-create the per-user
directory on such volumes. This is deliberately fixed in deployment rather
than by relaxing the ownership test in code, because relaxing it reintroduces
the shared-directory problem above.

**Why runtime resolution rather than configuration.** It requires no site map,
survives heterogeneous hosts, needs no update when mounts are added or
removed, and — usefully — encodes nothing site-specific in the source.

**Caching.** The upward walk costs several `stat(2)` calls, which on network
filesystems are round trips. Results are cached in a small fixed table so the
walk happens at most once per filesystem per thread.

Two constraints on the cache, both from review:

- **`st_dev` alone is not a sufficient key.** The resolved root is
  path-dependent: one filesystem can hold several user-owned subtrees beneath
  different group-owned parents, and whichever path resolves first would
  otherwise supply a root that is not an ancestor of later paths — silently
  invalidating the containment decision. A cache hit is therefore accepted
  only if the cached root is still an ancestor of the current path; otherwise
  the walk re-runs.
- **The cache is thread-local**, like the existing journal descriptor
  (`undo_shim.c:50`). A process-wide table would need locking, and taking a
  lock inside an interposer invites deadlock and leaves undefined state across
  `fork`. Per-thread walks cost more `stat` calls than a shared table but
  avoid both hazards, and the existing unsynchronized `dedup_tab`
  (`undo_shim.c:344`) is a precedent worth not repeating.

**Accepted risk: TOCTOU.** The resolver stats `P`, walks its parents, then
links or opens `P`. Components can be renamed, replaced, or mounted over in
between, so a backup may land on a different filesystem than the inode that is
ultimately destroyed. The existing code already has a pre-operation race
(`undo_shim.c:380`); this widens it. Closing it properly needs directory-fd
resolution and device revalidation throughout, which is a larger change than
this design takes on. Documented, not solved.

**Implementation trap.** `mkdir` is itself interposed by the shim. Creating a
store directory must happen with the `in_shim` guard held, or undo will
journal its own bookkeeping as user activity.

### 3. Reflink-aware copying: a three-tier ladder

For in-place overwrites, try progressively cheaper mechanisms:

1. **`ioctl(out_fd, FICLONE, in_fd)`** — success means the filesystem shared
   the extents, so the operation was constant-time and allocated nothing
   *immediately*. **The size cap still applies**, because the original is
   about to be overwritten and the shared extents will diverge: the allocation
   is deferred, not avoided. Tier 1 buys latency and avoids a network round
   trip; it does not make a large backup free. See item 4.
2. **`copy_file_range(2)`** — may offload the copy to the server, avoiding a
   round trip through the client, but still allocates a full second copy.
   Saves time and network, not space, so **the cap still applies**.
3. **read/write loop** — the universal fallback. **Cap applies.**

All three tiers are subject to the size cap; they differ in latency and in
whether bytes cross the network. Only a *hardlink* — used for deletions, where
the original inode survives untouched — is genuinely free, and hardlinks are
not part of this ladder.

Tier 2 is worth keeping even though it is useless on some filesystems: on
network filesystems with server-side copy support but no reflink — object
stores in particular — it is the only acceleration available, and it is
measurably faster than tier 3 there.

**Why `FICLONE` and not `copy_file_range`.** `copy_file_range` is the more
obvious choice but is unsuitable here: on network filesystems without
server-side copy offload it succeeds while performing an ordinary client-side
copy. Its return value therefore cannot distinguish a free clone from an
expensive copy, which is exactly the distinction needed to decide whether the
size cap applies. `FICLONE` either reflinks or fails, so its result is
meaningful. Measured on reflink-capable local storage, `FICLONE` was roughly
two orders of magnitude faster than a read/write copy; on an NFS mount,
`copy_file_range` showed no improvement over read/write at all.

`ioctl` does not raise the glibc symbol floor.

### 4. GC accounting by recorded save method

An earlier version of this design classified backups at GC time by
`st_nlink` — treating `nlink > 1` as a hardlink and `nlink == 1` as a copy.
**That is wrong and was removed.** The shim hardlinks the file and then the
real `unlink` immediately removes the original, so the link count returns to 1
before GC ever runs. A hardlink backup is indistinguishable from a copy by the
time anything inspects it. Reflinked backups also have `nlink == 1`.

The save method is knowable only at save time, so the shim records it:

| Method | Allocates | Budget treatment |
|---|---|---|
| hardlink | nothing | excluded from the size budget |
| reflink (`FICLONE`) | nothing *initially* | counted at full logical size |
| byte copy | full size | counted at full logical size |

**Why reflinks are counted at full size.** A reflink shares extents with the
original — but undo's use case is to back up a file that is *about to be
overwritten*. As the original is rewritten the extents diverge and the space
is really consumed. Treating a reflink as free would let the store silently
grow past its budget. The clone buys latency and avoids a network round trip;
it does not reliably avoid allocation. Counting pessimistically is the safe
error.

`UNDO_MAX_STORE` therefore applies to reflinked and copied bytes. Hardlinked
bytes are governed by session count and by a new `UNDO_MAX_AGE` time limit,
since their cost is deferred reclamation rather than allocation.

Without this, the size budget makes the hardlink mechanism useless for exactly
the large files it exists to protect: a hardlinked backup counted at full size
is evicted on the next command.

**Orphan recovery.** Distributed backups are deleted by reading the session's
journal, so a lost or truncated journal orphans them beyond the reach of GC.
`undo gc` gains a sweep that removes `.undo/<session-id>/` directories under
known store roots whose session no longer exists. Without it, storage leaks
are unrecoverable without manual cleanup.

### 5. Make unprotected files visible

When a file exceeds the copy budget, undo skips it and **says so**. The `lost`
record already exists in the journal; it must be surfaced:

- after `undo run`, as a count of unprotected files;
- in `undo list`, marking affected sessions;
- in the pre-revert preview, before the user relies on a restore.

Deliberately **not** emitted from the shim. Writing to stderr from inside
every process a user runs risks corrupting output that scripts parse. The
shim records; the CLI reports.

### 6. Per-volume `doctor` checks

Extend `undo doctor` to resolve the store for the current directory, prove
`link(2)` works into it, and report the effective budget and whether `FICLONE`
is available there. On a heterogeneous host this is the diagnostic that
matters most, because the answer legitimately differs per directory.

## Invariants

These hold for every change:

1. **The shim must never cause the user's command to fail.** All internal
   errors are swallowed; the real syscall's result is returned untouched.
   Upstream honors this; each new failure path introduced here is a new
   opportunity to violate it, so it is asserted by test.
2. **No new libc call may raise the glibc symbol floor** above the oldest
   supported host generation. Upstream keeps a hand-rolled integer parser
   specifically because `strtoul` under `_GNU_SOURCE` redirects to a symbol
   versioned newer than some supported distributions; a shim built against a
   newer glibc then refuses to load on older hosts. Verified: a shim calling
   `strtoul` built on a recent glibc requires `GLIBC_2.38`, while the same
   shim without it requires only `GLIBC_2.34`. CI must assert the floor.

   **Corollary for heterogeneous fleets.** Build against the oldest *widely
   deployed* generation, not the newest and not the absolute oldest. Nodes
   older than that floor, or on architectures upstream does not ship binaries
   for, need a source build or explicit exclusion. State the resulting
   coverage as a number rather than assuming the fleet is uniform — a shim
   that silently fails to load produces exactly the symptom undo is meant to
   eliminate, a user who believes they are protected and is not.
3. **No site-specific data in this repository.** See below.

## Degradation ladder

Every failure falls back to something safe, and every irreversible degradation
is reported:

| Failure | Falls back to | Visible? |
|---|---|---|
| Store resolution finds no owned ancestor | `$UNDO_SESSION/data` | doctor |
| `link()` returns `EXDEV` | byte copy | no (still protected) |
| `FICLONE` unsupported | read/write copy, capped | no (still protected) |
| File exceeds cap | nothing saved, `lost` record | **yes, loudly** |
| Store unwritable or quota exhausted | nothing saved, `lost` record | **yes, loudly** |

## Testing

- **Regression test for the `rmdir` guard leak**, from the confirmed
  reproduction: failing `rmdir()` followed by `unlink()` in one process must
  journal the unlink.
- **Multi-filesystem harness.** Two loopback filesystem images in a container
  give distinct `st_dev` without needing a cluster, and are reproducible by
  anyone. Covers store placement, `EXDEV` fallback, and GC across distributed
  stores.
- **Reflink path** on a reflink-capable loopback image; assert the cap is
  bypassed only when `FICLONE` actually succeeded.
- **Containment guard**, asserting that a recursive delete of the directory
  that would otherwise host the store falls back to the session directory
  instead of writing backups into the tree being destroyed; plus the boundary
  cases — `/x/a` vs `/x/ab`, paths containing `..`, and a fallback that itself
  lies under the operated tree.
- **`rmdir` must still succeed** when the only thing left in the target
  directory is undo's own store. This is the concrete form of Invariant 1 and
  the one most likely to regress.
- **Save-method recording**, asserting a hardlinked backup is still identified
  as a hardlink after the original is unlinked — the failure that killed the
  `st_nlink` approach.
- **Cross-device restore**, exercising every `os.Rename` call site in
  `restore.go` with the store on a different filesystem from the target,
  including the exchange and directory-parking paths that currently bypass
  `moveAny`.
- **Orphan sweep**, asserting `undo gc` reclaims distributed backups whose
  session journal was deleted.
- **Accounting**, asserting a large hardlinked backup survives GC while an
  equally large copied backup is pruned.
- **Invariant test:** with the store unwritable, every interposed call must
  still return the real syscall's result.
- **glibc floor assertion** in CI, extending upstream's existing check.

## Upstream contribution

The `rmdir` guard fix is submitted upstream on its own, from a personal fork,
before any of the multi-filesystem work. It is a self-contained bug fix with
no site relevance and no dependency on the rest of this design.

Upstream's `CONTRIBUTING.md` requires that any change to the shim come with an
end-to-end case, and states the shim's contract as: no output on the happy
path, no aborts, fail open when anything goes wrong. The regression test
described under Testing satisfies the first; the contract is already captured
as Invariant 1, and it is the reason the `lost` warning in item 5 is emitted
by the CLI rather than the shim.

Remaining items in this design are site-motivated but not site-specific, and
may be offered upstream later on their merits. Nothing here requires it.

## Phasing

1. `rmdir` fix (upstream PR) + fork skeleton + multi-filesystem test harness
   + secret-scanning hook (below).
2. Store placement + GC accounting. These two carry the actual value; after
   this phase the tool is correct on multi-filesystem hosts.
3. `FICLONE` + visible `lost` + per-volume `doctor`.
4. Agent integration skill (separate design).

## Repository hygiene

Work is split across two remotes by sensitivity:

- **Public fork** (GitHub, personal fork of upstream) — the shim and CLI
  changes, generic tests, this document. Site-agnostic; the source of upstream
  pull requests.
- **Private repository** (self-hosted, access limited to the owning group) —
  deployment configuration, per-volume budgets, real paths, and measurement
  data.

The public repository must contain no site-specific information: no hostnames,
mount points, organization domains, internal addresses, usernames, or storage
capacities. The design above requires none — store placement is computed at
runtime, and tests build their own synthetic filesystems.

Enforcement is mechanical, not procedural: a pre-push hook and a CI job fail
on a denylist of site identifiers. Discipline alone is not sufficient, because
the natural way to debug this code is to paste real paths into it.

Measurements may be published in generalized form ("on an NFS mount backed by
ZFS, `copy_file_range` showed no server-side offload") but never attributed to
named hosts or volumes.

## Non-goals

- **Concurrent journal writes from multiple hosts.** The shim appends journal
  lines with a single `write(2)` to an `O_APPEND` descriptor. This is
  effectively atomic on local filesystems but NFS provides no atomic-append
  guarantee, so multi-host jobs sharing one session could interleave records.
  Out of scope: the supported model is interactive and agent-invoked commands.
  Documented as a limitation.
- **Static binaries and programs making raw syscalls** (including Go
  binaries). An architectural limit of `LD_PRELOAD`, not fixable here.
- **Changing reclamation semantics.** Deleting a file whose backup is held
  does not free quota until that backup is pruned. This is inherent to
  hardlinks and must be documented rather than engineered around.
