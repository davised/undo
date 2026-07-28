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

The journal already records backup locations as absolute paths, and the Go
restore path already tolerates cross-device moves. **The storage format needs
no change** to support distributed backups — only the code that chooses where
a backup goes.

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
4. That ancestor is the store root; backups go to
   `<ancestor>/.undo/<session-id>/`.
5. If no such ancestor exists, return failure and fall back to
   `$UNDO_SESSION/data` (upstream behavior).

**Why "owned by the caller" rather than merely writable.** On shared storage
the mount root is typically not user-writable, while a directory the user owns
inside the volume is. Selecting the highest *owned* ancestor lands on the
user's own top-level directory on that volume, which is both writable and a
natural place for a private store. Selecting merely the highest *writable*
ancestor risks landing in a shared directory.

**Why runtime resolution rather than configuration.** It requires no site map,
survives heterogeneous hosts, needs no update when mounts are added or
removed, and — usefully — encodes nothing site-specific in the source.

**Caching.** The upward walk costs several `stat(2)` calls, which on network
filesystems are round trips. Results are cached per `st_dev` in a small fixed
table, so the walk happens at most once per filesystem per process.

**Implementation trap.** `mkdir` is itself interposed by the shim. Creating a
store directory must happen with the `in_shim` guard held, or undo will
journal its own bookkeeping as user activity.

### 3. Reflink-aware copying via `FICLONE`

For in-place overwrites, attempt `ioctl(out_fd, FICLONE, in_fd)` before
falling back to a read/write loop.

- **Success** means the filesystem cloned the extents: the copy allocated
  nothing and took constant time. The size cap does not apply.
- **Failure** (`EOPNOTSUPP`, `EXDEV`, `EINVAL`) means a real byte copy is
  required. Fall back to the read/write loop **with** the cap applied.

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

### 4. Split GC accounting by link count

Classify each backup by `st_nlink`:

- `nlink > 1` — **held**. A hardlink; allocates nothing. Its cost is deferred
  reclamation, not disk consumption.
- `nlink == 1` — **copied**. Real allocated bytes.

`UNDO_MAX_STORE` applies to **copied** bytes only. Held bytes are governed
separately, by session count and by a new age limit.

Add `UNDO_MAX_AGE` for time-based expiry. On scratch-like storage "keep four
hours" bounds the deferred-reclamation surprise more naturally than "keep
thirty commands".

Without this change, item 2 above makes the hardlink mechanism useless for
exactly the large files it exists to protect.

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
