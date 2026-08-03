# Per-volume `undo doctor`: design

**Status:** proposed
**Date:** 2026-08-02
**Implements:** item 6 of `undo-multifs-design.md`

Site-agnostic by construction: it names classes of filesystem, never a host,
mount or organization. See "Repository hygiene" in the multi-filesystem design.

## Summary

`undo doctor` proves the install works by deleting and restoring a real canary
rather than guessing. On a host with one filesystem that is a complete answer.
On a host with many, it is a confident answer about the wrong one.

This makes the check answer the question the user actually has — "is my work
protected *here*" — for the current directory and for any other directories
named on the command line.

## The problem, precisely

`reportRoundTrip` (`cmd/undo/doctor.go`) calls `os.MkdirTemp("", ...)`. The
empty first argument means `$TMPDIR`, falling back to `/tmp`. Everything the
check subsequently proves — that the shim loads, that a deletion is captured,
that a restore returns the bytes — it proves about whatever `$TMPDIR` names.

That is never the volume in question, and on a cluster it is not even
predictable: batch systems routinely set `TMPDIR` per job to node-local
scratch. The check reports on a filesystem chosen by the environment, with no
relationship to where the user keeps work.

Since backups became filesystem-local (phase 2b), the properties that decide
whether undo protects anything are per-volume:

- whether the store resolves to a directory the user owns, or falls back to the
  session store;
- whether `link(2)` into that store succeeds, which is the difference between a
  deletion costing nothing and costing a full capped copy;
- whether the volume supports reflink, which decides whether a large in-place
  overwrite could ever be cheap.

None of these are knowable from a `$TMPDIR` result, and on a heterogeneous host
they legitimately differ between two directories the same user owns.

## Design

### Command surface

```
undo doctor                 install checks, then a volume section for the cwd
undo doctor <path>...       install checks once, then one section per path
```

`cmdDoctor` currently takes no arguments and `main.go` discards `args[1:]`; it
gains a `[]string` parameter. With none given the target is the working
directory as `os.Getwd` reports it.

Each argument is reported under the name the user typed, because that is the
name they will recognise. It is **canonicalized before anything is probed**:
`filepath.EvalSymlinks` then `filepath.Abs`, and the canary is created in, and
passed as, that canonical directory.

That is not tidiness, it is required for a correct answer. The shim does not
call `realpath` — `abs_path` keeps the lexical path — and `resolve_store_root`
then walks *lexical* parents while `stat` follows each final component. For
`/link/canary`, where `/link` is a symlink onto another filesystem, the walk can
select `/link` and stop immediately at `/` because the device changed, never
visiting the real parents at all. The answer for the symlink spelling and the
answer for the real path differ, and only the latter describes the volume. A
home directory that is a symlink onto another volume is a common enough layout
that this is the normal case, not an edge one.

Canonicalizing also settles the journal match below: the victim handed to the
deletion must be the same absolute lexical string the shim records, which a
relative target would not produce.

One section per argument, in the order given, even when two arguments land on
the same filesystem: the user asked about two directories and deduplicating
would answer a question they did not ask. Where two sections resolve to the
same store root, say so rather than repeating the work silently.

### The round trip moves; the old location becomes a control

The live capture/restore is not duplicated. It **runs in the target volume**
instead of `$TMPDIR`.

`$TMPDIR` keeps a smaller role: when a target fails *at capture or restore*,
the same round trip is retried there as a control, to narrow the diagnosis:

| target | control | reading |
|---|---|---|
| fails | passes | the shim works where it was tried; this volume is the difference |
| fails | fails | the shim is not working anywhere it was tried; the volume is not implicated |

This narrows a diagnosis; it does not prove causation. Both locations carry
their own permissions, mount options, ignore rules and preload restrictions,
and either can fail for reasons that are nothing to do with the other.

The control runs **at most once per invocation** and its result is reused
across targets. It is not run for failures that never reached the shim — a
missing path, or a target that could not be written — since those say nothing
about capture.

### Per-volume procedure

For each target directory:

1. Create the canary with `os.CreateTemp(canonicalTarget, ".undo-doctor-")` —
   **a file, directly in the target directory, never inside a subdirectory the
   doctor creates.** See below; this is the single most important constraint in
   this design.
2. Create a session. Delete the canary through the shim, armed exactly as the
   existing round trip arms it.
3. Read the journal and classify (below).
4. Restore, and compare the recovered bytes with what was written.
5. Probe reflink support between two files created the same way in the target.
6. Remove anything the doctor created that still exists.

Step 6 needs stating precisely, because step 4 deliberately puts the canary
back. Cleanup runs **after** classification and verification, removes only
paths this run created, and runs **without the armed environment** — deleting
the canary under the shim would journal a second operation, and with lazy
session creation it could mint a whole extra session as a side effect of a
diagnostic. Cleanup failures are reported, not swallowed, but do not change the
volume's verdict: the answer was already obtained.

### Why the canary is not placed in a temp subdirectory

An earlier revision of this design created `<target>/.undo-doctor-XXXX/` and
put the canary inside it. **That is wrong, and it fails in exactly the case
this feature exists to detect.**

`resolve_store_root` walks upward from the file and takes the *highest*
ancestor that is on the same device, owned by the effective uid, and writable.
A temp directory the doctor just created is owned by the caller by
construction. On a volume where the user owns nothing above the target — the
group-owned-parent layout the multi-filesystem design calls out as the case
requiring per-user directories to be pre-created — the walk would find no
qualifying ancestor at all for a real file, and fall back to the session store
with capped copies. With a doctor-owned directory in the path it instead finds
one: the doctor's own temp directory. A store is created there, the hardlink
succeeds, and `doctor` reports free deletions and a healthy store root for a
directory in which the user's real files are not protected that way.

The containment guard does not save this. It rejects a candidate that is the
operated-on path or a descendant of it; the temp directory is the canary's
*parent*, so nothing rejects it.

A canary created directly in the target has the same set of ancestors as the
user's own files there, which is the only arrangement whose answer transfers.

The cost is a file appearing briefly in the user's directory rather than in a
subdirectory. `os.CreateTemp` gives it a unique name, so concurrent runs do not
collide, and it is removed on every exit path.

### Classifying the result

`journal.Read`'s error is currently discarded (`entries, _ :=`). It must not
be: this design reads fields out of the journal to decide what to report, so a
truncated or unreadable journal would otherwise surface as "nothing was
recorded", which is a different and much more alarming diagnosis than the truth.

The canary's record is selected **by operation and exact path**, not by
position. Journals legitimately carry other records, `storemv` among them, and
records that failed an integrity check keep their slots rather than being
filtered out, so an index-based rule is wrong.

Two operations match, and accepting only the first would miss the case most
worth reporting. When the backup fails, the shim writes `lost <victim> unlink`
*instead of* an `unlink` record — so a rule that looks only for `OpUnlink` goes
blind exactly when nothing was saved. The match is:

- `OpUnlink` whose first field is the canary, or
- `OpLost` whose first field is the canary and whose second is `unlink`.

If no entry matches, that is the "nothing recorded" case. If more than one
matches, that is itself a defect worth surfacing — one deletion produced two
records — and it is reported rather than resolved by taking the first or the
last, either of which would be a guess.

A matching record with `Corrupt` set is reported as journal corruption. Its
fields are not used to classify, and nothing is restored from it: the whole
point of the integrity field is that those fields are not trustworthy.

From the selected entry:

| Observation | Reported as |
|---|---|
| backup path contains a `/.undo/` component | store root is the path truncated at it |
| backup path lies within the session directory | session-store fallback |
| `Method() == "link"` | deletions are free here |
| `Method() == "copy"` | deletions cost real bytes |
| `Method() == "none"`, or an `OpLost` record | nothing was saved for this file |
| any other method token | printed verbatim, not silently mapped |

`Method` returns `link`, `copy` or `none`; `lost` is an *op*, not a method, and
the two must not be conflated. An unrecognised token is printed as-is so that a
future save method does not read as a failure.

Truncation at `/.undo/` is a structural claim about a path this process just
caused to be written, so it is checked rather than assumed: the component must
be present, what precedes it must be non-empty, and the component that follows
must be **this run's own session id**, not merely something session-id shaped.
The weaker test would accept an unrelated `.undo`-shaped path elsewhere in the
tree and report it as this session's store. Anything failing the check is
reported as an unexpected backup location rather than parsed into a store root.

Classification happens **before** the session is removed. `Session.Remove`
deletes backups outside the session directory by reading the journal, so the
paths being classified are the paths it is about to delete.

### What each volume section reports

| Field | Source | Note |
|---|---|---|
| store root | journal backup path | or "session-store fallback" |
| deletions | `Entry.Method()` | free, or costing real bytes |
| overwrites | `UNDO_MAX_BYTES` | the per-file cap for in-place writes |
| reflink | `FICLONE` probe | **labelled as not yet used by the shim** |
| budget | `UNDO_MAX_STORE` | **labelled global, not per-volume** |

Two labels are load-bearing and must not be dropped for brevity:

- **Reflink is reported but unused.** The copy ladder is phase 3 and not
  implemented. Reporting "reflink: supported" without that qualifier would tell
  a user that large overwrites are cheap here when the shim will still take a
  capped byte copy. It is reported anyway because it is a real, stable property
  of the volume and one of the inputs to choosing a budget.
- **The budget is global.** `UNDO_MAX_STORE` is a single number for the whole
  store, and printing it inside a per-volume section implies a per-volume
  budget that does not exist. Whether it should is an open question and a
  separate decision; this design must not prejudge it by displaying something
  that reads as already per-volume.

`copy` is a warning, but the reason is not inferable from the method alone: it
may follow a fallback, or a hardlink that failed despite a usable local store.
The section reports the store root beside it so the two are distinguishable,
and does not guess which occurred.

### The reflink probe

`FICLONE` is an `ioctl`. `go.mod` requires no external modules, and pulling in
`golang.org/x/sys` for one request number would make this the project's first
dependency; it is done with `syscall.Syscall` and the constant written out and
documented.

It takes **two** files, not one. A `//go:build linux` file holds the real
probe, and a `//go:build !linux` file defines the same function returning "not
available on this platform". A Linux-only file alone does not make the package
build elsewhere — it makes the call site undefined — and the workstation these
agents run on is macOS, where `gofmt` and `go vet` are expected to work.

The probe runs between two files in the target directory, so it describes the
target's filesystem. That is the store's filesystem in every case except the
fallback, where the store is on the session store's filesystem instead — so
when the fallback is reported, the reflink line is labelled as describing the
target rather than the store.

Errno mapping is explicit, because "unsupported" and "the probe itself broke"
are different answers: `EOPNOTSUPP`, `ENOTTY` and `EINVAL` report unsupported;
`EXDEV` is a bug in the probe, since both files are created in one directory,
and is reported as an error rather than as unsupported; anything else is
reported as an error with the errno.

### Failure states

Every one is a report, never a crash, and never aborts the remaining targets:

| Condition | State | What is said |
|---|---|---|
| target missing or not a directory | FAIL | that target only; no control |
| target not writable | FAIL | cannot protect anything here; no control |
| store resolved to the session-dir fallback | warn | no owned ancestor on this volume; backups are capped copies. Pre-creating a directory owned by the user on that volume fixes it |
| shim recorded nothing here, control passes | FAIL | this volume |
| shim recorded nothing here, control also fails | FAIL | the shim, not the volume |
| method is `none`, or an `OpLost` record | warn | nothing was saved for that file |
| restore returned different bytes | FAIL | this volume |
| the matching record is corrupt | FAIL | the journal, and nothing is restored from it |
| more than one record matches the canary | FAIL | one deletion produced two records |
| target is a symlink that cannot be resolved | FAIL | that target only; no control |

The fallback warning matters more than its severity suggests. It is the
documented deployment consequence of the placement algorithm: where per-user
directories sit beneath a parent the user does not own, the walk finds no owned
ancestor and undo silently degrades to capped copies. `doctor` is the only
place a user finds that out before relying on a restore — and it is the case
the temp-subdirectory mistake above would have hidden.

Exit status remains worst-of-all-checks across the install checks, every
target, and the control.

### Two existing defects this touches

Both are in code this design rewrites, and both are fixed here rather than
left behind it:

- **The store writability probe uses a fixed filename**, `.doctor-probe`. Two
  concurrent doctors overwrite and remove each other's file; one can report
  success on the strength of the other's. It becomes a `CreateTemp` file that
  each run removes only its own copy of.
- **`journal.Read`'s error is discarded**, as above.

### Output compatibility

`test/e2e.sh` case 23 asserts the literal strings `[ok  ] capture` and
`[ok  ] restore`. The round trip that produces them is the one being moved, so
those names are kept for the primary target and the per-volume detail is
reported alongside them. Renaming them would be a gratuitous break of a case
that is testing the right thing.

### Constraints

- **No new module dependencies**, as above.
- **No shell interpolation of a target path.** The existing round trip builds
  `exec.Command("/bin/sh", "-c", "rm "+victim)`. Once a user-supplied path
  reaches `victim`, a space or a shell metacharacter makes that wrong, and in
  the worst case makes it execute something else. The victim is passed as an
  argument, not concatenated into a script.
- **No shim change**, so the glibc floor is untouched and `CONTRIBUTING.md`'s
  "an e2e case per shim change" does not apply — though cases are added anyway.
- **Nothing site-specific**, per the multi-filesystem design.

## Testing

- **Normal volume.** A target whose store resolves reports a store root and
  `link`.
- **Group-owned parent.** A target the user does not own, beneath a parent they
  do not own, reports the session-store fallback and warns. This is the case
  the temp-subdirectory placement would have reported as healthy, so it is the
  regression test for the central correction in this design.
- **Unwritable target.** FAIL for that target, with the remaining targets still
  checked.
- **Missing path.** Fails cleanly; the remaining targets are unaffected.
- **A path containing a space.** Proves the victim is not interpolated into a
  shell script.
- **A target reached through a symlink onto another filesystem.** Reports the
  same store root as the real path does, which is what canonicalizing buys.
- **A failed backup.** The record is `lost <victim> unlink`, not `unlink`, and
  the volume is still classified rather than reported as "nothing recorded".
- **Two filesystems.** `test/multifs.sh` already builds them, so the assertion
  that two targets report two different store roots is real rather than
  simulated. This is the case that would have caught the original bug.
- **Control path.** With the shim unloadable, a target failure attributes the
  cause to the shim rather than the volume.
- **`TMPDIR` set elsewhere.** The check follows the target, not the
  environment.
- **Case 23 still passes** unmodified.

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
