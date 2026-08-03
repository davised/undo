# Per-volume `undo doctor`: design

**Status:** proposed
**Date:** 2026-08-02
**Implements:** item 6 of `undo-multifs-design.md`

Site-agnostic by construction: it names classes of filesystem, never a host,
mount or organization. See "Repository hygiene" in the multi-filesystem design.

## Summary

`undo doctor` proves the install works by deleting and restoring a real canary
rather than guessing. On a host with one filesystem that is a complete answer.
On a host with many, it is a confident answer about the wrong one: the round
trip runs in `/tmp`, so it reports on node-local storage while the user's data
sits on a network filesystem whose behaviour differs in every way that matters.

This makes the check answer the question the user actually has — "is my work
protected *here*" — for the current directory and for any other directories
named on the command line.

## The problem, precisely

`reportRoundTrip` (`cmd/undo/doctor.go`) calls `os.MkdirTemp("", ...)`. The
empty first argument means `$TMPDIR`, in practice `/tmp`. Everything the check
subsequently proves — that the shim loads, that a deletion is captured, that a
restore returns the bytes — it proves about `/tmp` and nothing else.

Since backups became filesystem-local (phase 2b), the properties that decide
whether undo protects anything are per-volume:

- whether the store resolves to a directory the user owns, or falls back to the
  session store;
- whether `link(2)` into that store succeeds, which is the difference between a
  deletion costing nothing and costing a full capped copy;
- whether the volume supports reflink, which decides whether a large in-place
  overwrite could ever be cheap.

None of these are knowable from a `/tmp` result, and on a heterogeneous host
they legitimately differ between two directories the same user owns.

## Design

### Command surface

```
undo doctor                 install checks, then a volume section for the cwd
undo doctor <path>...       install checks once, then one section per path
```

The install checks (shim located, libc flavour, store present and private,
ignore configuration, hooks active) are host-wide and run once regardless.

### The round trip moves; `/tmp` becomes a control

The live capture/restore is not duplicated. It **runs in the target volume**
instead of `/tmp`.

`/tmp` keeps a role, but a different and smaller one: when a target fails, the
same round trip is retried there as a control. That is what separates the two
diagnoses a user cannot otherwise tell apart:

| target | control | verdict |
|---|---|---|
| fails | passes | the shim works; this volume cannot be protected |
| fails | fails | the shim is not working at all; the volume is not the problem |

Running the control only on failure keeps the common path at one round trip.

### Per-volume procedure

For each target directory:

1. `os.MkdirTemp(target, ".undo-doctor-")` — inside the target, so the canary
   is on the filesystem being asked about, and so an unwritable target fails
   here with a clear reason rather than misreporting later.
2. Write a canary file. Create a session. Delete the canary through a shell
   with the shim armed, exactly as the existing round trip does.
3. Read the journal. Two fields carry the answer:
   - the backup path, truncated at its `/.undo/` component, **is** the resolved
     store root;
   - `Entry.Method()` reports `link`, `copy` or `lost`.
4. Restore, and compare the recovered bytes with what was written.
5. Probe reflink support between two small files in the same temp directory.
6. Remove the temp directory.

### Why the store root is read back rather than predicted

The obvious alternative is to reimplement the upward walk from
`resolve_store_root` in Go and report what it would choose. That is rejected.

The walk is load-bearing and subtle — it depends on `st_dev` boundaries,
effective uid, the write bit, a containment guard, and a cache whose key is not
`st_dev` alone. A second implementation would have to track every one of those
decisions forever, and the failure mode when it drifts is a `doctor` that
confidently reports a store root the shim does not use. This codebase has
already produced one class of bug where a check passed while the thing it
checked was not happening; a predicted store root is an invitation to another.

Reading the path back out of the journal has no such failure mode. Whatever the
shim did is what gets reported, because the shim is what wrote it.

The cost is that `doctor` must perform a real write in the target directory.
That is consistent with what the command already is — its value has always been
that it does the thing rather than reasoning about it.

### What each volume section reports

| Field | Source | Note |
|---|---|---|
| store root | journal backup path, truncated at `/.undo/` | or "session-store fallback" |
| deletions | `Entry.Method() == "link"` | free, or costing real bytes |
| overwrites | `UNDO_MAX_BYTES` | the per-file cap for in-place writes |
| reflink | `FICLONE` probe | **labelled as not yet used by the shim** |
| budget | `UNDO_MAX_STORE` | **labelled global, not per-volume** |

Two labels are load-bearing and must not be dropped for brevity:

- **Reflink is reported but unused.** The copy ladder is phase 3 and not
  implemented. Reporting "reflink: supported" without that qualifier would tell
  a user that large overwrites are cheap here when the shim will still take a
  capped byte copy. It is reported anyway because it is a real, stable property
  of the volume and it is one of the inputs to choosing a budget.
- **The budget is global.** `UNDO_MAX_STORE` is a single number for the whole
  store, and printing it inside a per-volume section implies a per-volume
  budget that does not exist. Whether it should is an open question and a
  separate decision; this design must not prejudge it by displaying something
  that reads as already per-volume.

### Failure states

Every one is a report, never a crash, and never aborts the remaining targets:

| Condition | State | What is said |
|---|---|---|
| target missing or not a directory | FAIL | that target only |
| target not writable | FAIL | cannot protect anything here |
| store resolved to the session-dir fallback | warn | no owned ancestor on this volume; backups are capped copies. Pre-creating a directory owned by the user on that volume fixes it |
| shim recorded nothing here, control passes | FAIL | this volume |
| shim recorded nothing anywhere | FAIL | the shim, not the volume |
| method is `lost` | warn | nothing was saved for that file |

The fallback warning matters more than its severity suggests. It is the
documented deployment consequence of the placement algorithm: where per-user
directories sit beneath a parent the user does not own, the walk finds no owned
ancestor and undo silently degrades to capped copies. `doctor` is the only
place a user finds that out before relying on a restore.

Exit status remains worst-of-all-checks, as today.

### Constraints

- **No new module dependencies.** `go.mod` currently requires nothing. The
  reflink probe is an `ioctl`, and pulling in `golang.org/x/sys` for it would
  make this the first dependency in the project. It is done with
  `syscall.Syscall` and the request number written out, in one small helper with
  the constant documented.
- **No shim change**, so the glibc floor is untouched and
  `CONTRIBUTING.md`'s "an e2e case per shim change" does not apply — though
  cases are added anyway, below.
- **Nothing site-specific**, per the multi-filesystem design.

## Testing

- **Normal volume.** `doctor` on a directory whose store resolves reports a
  store root and `link`.
- **Fallback volume.** A target whose store cannot be created reports the
  fallback and warns, rather than reporting success.
- **Unwritable target.** Reports FAIL for that target and still checks the
  others.
- **Missing path.** Fails cleanly, with the remaining targets unaffected.
- **Two filesystems.** `test/multifs.sh` already builds them, so the assertion
  that two targets report two different store roots is real rather than
  simulated. This is the case that would have caught the original bug.
- **Control path.** With the shim unloadable, a target failure reports the shim
  as the cause rather than the volume.

## Non-goals

- **Quota reporting.** There is no portable way to query a per-user quota from
  a shell on the filesystems in question, and a wrong number is worse than
  none.
- **Enumerating every mount.** A host may carry on the order of a hundred
  per-user or per-group mounts. Probing all of them would be slow, would mostly
  report volumes the user has no interest in, and would produce failures for
  directories they cannot write to and should not care about.
- **Per-volume budgets.** Reported as global here precisely because that is
  what they are. Changing it is a separate decision with its own design.
- **Fixing the fallback.** `doctor` reports that a volume has no owned
  ancestor; creating the directory that would fix it is a deployment action,
  deliberately not something the tool does to the user's filesystem.
