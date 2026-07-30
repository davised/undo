# undo multi-filesystem — recording the loss of another session's store

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a command destroys a backup store belonging to a *different* session, say so in that session's journal — so the last silent loss in the tool stops being silent.

**Architecture:** The shim already notices when something deletes a backup inside its own store and evacuates it. It ignores stores belonging to other sessions, because it cannot evacuate those: the copies would land in the wrong session's directory and the records would go in the wrong journal. Instead it appends `storemv <path> -` to the *other* session's journal, which is a sibling directory of `$UNDO_SESSION` and therefore derivable. Every consumer already understands that record.

**Tech Stack:** C (the `LD_PRELOAD` shim, glibc floor 2.34), bash (e2e suite).

## Why this and not evacuation

Evacuating a foreign store means copying its backups somewhere and telling its
session where they went. Both halves are wrong from inside another session: the
only directory the shim can write copies into is its own session's `data/`,
which the other session will never look in, and sizing that copy against
`UNDO_MAX_BYTES` charges one command for another's history.

Recording costs one open and one write per lost backup, needs no copy, and
removes the property that actually matters. The backups are still lost — but
`undo list` marks the session, the pre-revert preview warns, and the restore
explains itself instead of failing on a vanished path.

It also composes with the orphan sweep in 2c: a store whose session has been
told its backups are gone gets reclaimed on the next `undo gc` rather than
lingering as an unreferenced directory.

## Known limitation: a live owning session

If the owning session is **still running**, its own `unlink` record can be
appended *after* our `storemv`. `ResolveStoreMoves` rewrites only entries that
precede a `storemv`, so that one record keeps pointing at a backup that is
already gone, and for that entry the loss stays silent.

This is accepted rather than fixed, on three grounds:

- **The fix would be a cross-process lock inside an interposer.** The design
  already rejects that for the resolver cache — "taking a lock inside an
  interposer invites deadlock and leaves undefined state across `fork`" — and
  the reasoning is stronger here, because the lock would have to span two
  processes writing two different journals.
- **It is not the case that actually happens.** A session is one command. A
  store being destroyed almost always belongs to an *earlier, finished*
  command, which is fully covered: its journal is complete before ours is
  written, so every record it holds precedes our `storemv`.
- **In the race we are no worse than today.** That entry remains silent, which
  is exactly the current behavior. Nothing recoverable is discarded and no
  wrong record is written — the improvement is simply absent for that one
  entry.

Reaching it requires a user deleting a tree while another shell is actively
writing backups into it, and losing a microsecond-wide window between the
hardlink and the journal append.

## Global Constraints

Copied from `docs/design/undo-multifs-design.md` and `AGENTS.md`.

- **The shim must never cause the user's command to fail.** This adds a write
  path inside `unlink`; it must not change the return value, `errno`, or
  whether the file was removed.
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`.
  The floor is currently exactly 2.34. `open`, `close`, `opendir`, `readdir`,
  and `closedir` are all already used by the shim.
- **No site-specific data in this repository.**
- **The journal format is append-only and additive.** This adds no new op and
  no new field — it writes an existing `storemv` record into a different file.
- **Upstream `CONTRIBUTING.md`:** anything touching the shim needs an e2e case.
- Every build and test runs through `test/in-container.sh`.

## The state being fixed

`rm -rf <store root>` removes `<root>/.undo/` entirely, including
`<root>/.undo/<other-session>/`. Today:

- `in_our_store` matches only the running session's id, so nothing fires.
- The other session's journal still names backups at paths that no longer exist.
- `Unprotected()` counts zero, because nothing was recorded.
- The user discovers it while trying to restore, as a bare "no such file".

Reproduced: two sessions, the first deleting a file under `<root>` and the
second running `rm -rf <root>`; afterwards `undo apply <first>` reports
`rename ... no such file or directory` and restores nothing.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `shim/undo_shim.c` | modify | `jwritev`/`jwrite_to`, `other_journal_fd`, `valid_session_id`, `in_any_store`; `handle_unlink_pre` notes a foreign store, `handle_unlink_post` records the loss once the unlink succeeded |
| `test/e2e.sh` | modify (append case 31) | a second session's backups are recorded as lost, and the CLI reports them |

## Interfaces produced

```c
static void jwritev(int fd, const char *op, va_list ap);   /* shared body */
static void jwrite_to(int fd, const char *op, ...);        /* to a given journal */
static int  other_journal_fd(const char *sid);             /* -1 unless that session exists */
static int  valid_session_id(const char *s, size_t len);   /* digits only, no . or .. */
static int  in_any_store(const char *abs, char *root_out, char *sid_out);
```

---

### Task 1: Let the shim write to another session's journal

`jwrite` is hardwired to `journal_fd()`, the current session's descriptor.
Splitting the formatting body out is what makes a second destination possible
without duplicating the percent-encoding, which must stay identical or the
reader will mis-decode paths.

**Files:**
- Modify: `shim/undo_shim.c` — `jwrite` (around line 98)

**Interfaces:**
- Consumes: `enc_append`, `journal_fd`.
- Produces: `jwritev`, `jwrite_to`.

- [ ] **Step 1: Refactor `jwrite`**

Replace the existing `jwrite` with:

```c
/* Formats and appends one record to an already-open journal descriptor.
 * Shared so a record destined for another session's journal is encoded by
 * exactly the same code -- a second copy of the percent-encoding is a second
 * chance for the two to disagree, and the reader would silently mis-decode. */
static void jwritev(int fd, const char *op, va_list ap)
{
    if (fd < 0)
        return;
    char line[4 * PATH_MAX];
    size_t len = 0;
    enc_append(line, sizeof line, &len, op);
    const char *f;
    while ((f = va_arg(ap, const char *)) != NULL) {
        if (len + 1 < sizeof line)
            line[len++] = '\t';
        enc_append(line, sizeof line, &len, f);
    }
    if (len + 1 < sizeof line)
        line[len++] = '\n';
    ssize_t r = write(fd, line, len);
    (void)r;
}

/* jwrite("op", field1, field2, NULL) -- to this session's journal */
static void jwrite(const char *op, ...)
{
    va_list ap;
    va_start(ap, op);
    jwritev(journal_fd(), op, ap);
    va_end(ap);
}

/* the same, to a journal the caller opened */
static void jwrite_to(int fd, const char *op, ...)
{
    va_list ap;
    va_start(ap, op);
    jwritev(fd, op, ap);
    va_end(ap);
}
```

- [ ] **Step 2: Build and run the suite**

Run: `test/in-container.sh make test`

Expected: all 30 cases pass. This step changes no behavior — it is a pure
refactor, and the suite is what proves that.

- [ ] **Step 3: Confirm the floor**

Run:

```bash
test/in-container.sh bash -c '
  gcc -shared -fPIC -O2 -Wall -Wextra -o /tmp/libundo.so shim/undo_shim.c -ldl
  objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
```

Expected: `GLIBC_2.34`.

- [ ] **Step 4: Commit**

```bash
git add shim/undo_shim.c
git commit -m 'shim: split the journal record formatter from its destination

jwrite was hardwired to journal_fd(), this session'"'"'s descriptor. A record
that belongs in another session'"'"'s journal needs the same encoding written to
a different file, and a second copy of the percent-encoding is a second
chance for the two to disagree -- with the reader silently mis-decoding paths
as the result.

Pure refactor: no behavior change, which the existing suite is what proves.'
```

---

### Task 2: Record the loss in the owning session's journal

**Files:**
- Modify: `shim/undo_shim.c` — near `in_our_store`, and `handle_unlink_pre`
- Test: `test/e2e.sh` (append case 31)

**Interfaces:**
- Consumes: `jwrite_to` from Task 1; `own_real_dir`, `session_dir`, `session_id`, `evac_seen`, `evac_mark`.
- Produces: `other_journal_fd`, `valid_session_id`, `in_any_store`.

- [ ] **Step 1: Write the failing e2e case**

Append to `test/e2e.sh`, before the closing `echo "all cases passed"`:

```bash
echo "== case 31: destroying another session's store is recorded, not silent"
mkdir -p "$PLAY/shared/work"
echo "first" >"$PLAY/shared/work/one.txt"
echo "second" >"$PLAY/shared/work/two.txt"

# session A deletes one file, which creates a store somewhere above it
a=$(date +%s%N | cut -c1-16); adir="$UNDO_DATA_DIR/sessions/$a"
mkdir -p "$adir/data"; echo "rm one" >"$adir/cmd"
env UNDO_SESSION="$adir" LD_PRELOAD="$LIB" bash -c "rm $PLAY/shared/work/one.txt"
sleep 0.01
abak=$(awk -F'\t' '$1=="unlink"{print $3}' "$adir/journal" | tail -1)
[[ -f $abak ]] || fail "session A took no backup: $(cat "$adir/journal")"

# session B deletes the whole store directory out from under it
store_root=${abak%/.undo/*}
b=$(date +%s%N | cut -c1-16); bdir="$UNDO_DATA_DIR/sessions/$b"
mkdir -p "$bdir/data"; echo "rm -rf store root" >"$bdir/cmd"
env UNDO_SESSION="$bdir" LD_PRELOAD="$LIB" bash -c "rm -rf $store_root/.undo"
sleep 0.01

[[ ! -e $abak ]] || fail "the backup survived; the test proves nothing"
grep -q "^storemv" "$adir/journal" ||
    fail "session A was not told its backup is gone: $(cat "$adir/journal")"

out=$("$UNDO" list)
grep -q "unprotected" <<<"$out" || fail "undo list did not flag session A: $out"

# A dot component must not let the derived journal path escape. abs_path does
# not normalize, so without valid_session_id the id here is ".." and the
# journal built from it lands one level above the sessions directory.
mkdir -p "$PLAY/trav/.undo"
echo probe >"$PLAY/trav/probe.txt"
c=$(date +%s%N | cut -c1-16); cdir="$UNDO_DATA_DIR/sessions/$c"
mkdir -p "$cdir/data"; echo "traversal probe" >"$cdir/cmd"
env UNDO_SESSION="$cdir" LD_PRELOAD="$LIB" \
    bash -c "rm $PLAY/trav/.undo/../probe.txt"
[[ ! -e $UNDO_DATA_DIR/journal ]] ||
    fail "a .. component created a journal outside the sessions directory"
```

- [ ] **Step 2: Run it and watch it fail**

Run: `test/in-container.sh bash -c 'make >/dev/null 2>&1 && test/e2e.sh'`

Expected: `FAIL: session A was not told its backup is gone`. If it fails on
`the backup survived` instead, the delete did not reach the store — print
`$store_root` and check the store really is under it.

- [ ] **Step 3: Implement**

In `shim/undo_shim.c`, insert after `in_our_store`:

```c
/* True for a string that is a plausible session id and nothing else.
 *
 * This is a security check, not a formatting one. The id is lifted out of a
 * path the shim was handed, and abs_path does no normalization, so without it
 * `rm <anything>/.undo/../x` yields an id of ".." -- and the journal path
 * derived from that resolves outside the sessions directory, where an ordinary
 * delete would then create a file. Requiring digits excludes "." and ".." and
 * "/" together, and session.Create only ever produces digits. */
static int valid_session_id(const char *s, size_t len)
{
    if (len == 0 || len > 64)
        return 0;
    for (size_t i = 0; i < len; i++)
        if (s[i] < '0' || s[i] > '9')
            return 0;
    return 1;
}

/* Fill root_out and sid_out for any backup path of the form
 * <root>/.undo/<session-id>/<name>, whoever it belongs to. The caller compares
 * sid_out against session_id() to decide whether the store is its own. */
static int in_any_store(const char *abs, char *root_out, char *sid_out)
{
    const char *p = abs;
    while ((p = strstr(p, "/.undo/")) != NULL) {
        const char *sid = p + 7; /* strlen("/.undo/") */
        const char *end = strchr(sid, '/');
        if (end && end > sid) {
            size_t rootlen = (size_t)(p - abs);
            size_t sidlen = (size_t)(end - sid);
            if (rootlen > 0 && rootlen < PATH_MAX && sidlen < PATH_MAX &&
                valid_session_id(sid, sidlen)) {
                memcpy(root_out, abs, rootlen);
                root_out[rootlen] = 0;
                memcpy(sid_out, sid, sidlen);
                sid_out[sidlen] = 0;
                return 1;
            }
        }
        p += 7;
    }
    return 0;
}

/* A descriptor for another session's journal, or -1.
 *
 * Sessions are siblings under one directory, so $UNDO_SESSION's parent plus
 * the id is the whole derivation -- the shim never learns UNDO_DATA_DIR and
 * does not need to. Returns -1 unless that session directory already exists
 * and is ours: this reports a loss to a session that is still there, and must
 * never bring one into being. */
static int other_journal_fd(const char *sid)
{
    const char *dir = session_dir();
    if (!dir)
        return -1;
    char base[PATH_MAX];
    if ((size_t)snprintf(base, sizeof base, "%s", dir) >= sizeof base)
        return -1;
    char *slash = strrchr(base, '/');
    if (!slash || slash == base)
        return -1;
    *slash = 0;

    char sdir[PATH_MAX], path[PATH_MAX];
    if ((size_t)snprintf(sdir, sizeof sdir, "%s/%s", base, sid) >= sizeof sdir)
        return -1;
    if (!own_real_dir(sdir))
        return -1;
    if ((size_t)snprintf(path, sizeof path, "%s/journal", sdir) >= sizeof path)
        return -1;
    REAL(open, int, const char *, int, ...);
    return real_open(path, O_WRONLY | O_APPEND | O_CREAT | O_CLOEXEC, 0600);
}

Then thread the owning session's id from `handle_unlink_pre` to
`handle_unlink_post`, and record the loss only once the real `unlink` has
actually succeeded.

**The record must not be written before the syscall.** A pre-call hook that
appends `storemv <path> -` is asserting a loss that has not happened yet: if the
unlink then fails -- permissions, a read-only filesystem, a race -- the backup
is still sitting there, but `ResolveStoreMoves` now rewrites it to `-` forever.
Restore would refuse perfectly good recovery data. Throwing away a usable backup
is a worse failure than the silent loss this plan exists to fix.

Recording per file rather than per store follows from the same reasoning. Marking
the whole store on the first backup deleted is a *prediction* that the rest will
go too, and a `rm -rf` that stops halfway makes that prediction wrong in the same
damaging direction. One record per backup actually removed is always accurate,
and it needs no enumeration and no once-per-store marker.

Give `handle_unlink_pre` a `char *foreign_sid` out-parameter, empty unless `abs`
is a backup belonging to another session:

```c
    *kind = 0;
    *method = "none";
    foreign_sid[0] = 0;
    if (abs_path(dirfd, path, abs) != 0)
        return;
    /* Something is deleting a backup. If it is ours, get the rest off this
     * filesystem now, while they still exist. If it belongs to another
     * session, note whose it is -- but record nothing until the unlink has
     * actually happened. */
    char root[PATH_MAX], sid[PATH_MAX];
    if (in_any_store(abs, root, sid)) {
        const char *mine = session_id();
        if (mine && strcmp(sid, mine) == 0)
            evacuate_store(root, 0);
        else
            snprintf(foreign_sid, PATH_MAX, "%s", sid);
    }
    if (ignored(abs))
        return;
```

and `handle_unlink_post` a matching parameter, writing the record only on
success:

```c
static void handle_unlink_post(int rc, const char *abs, const char *bak,
                               const char *lnk, int kind, const char *method,
                               const char *foreign_sid)
{
    if (rc == 0) {
        if (foreign_sid[0]) {
            /* This backup belonged to another session and is now gone. Its
             * own journal is the only place its reader will look. */
            int fd = other_journal_fd(foreign_sid);
            if (fd >= 0) {
                jwrite_to(fd, "storemv", abs, "-", NULL);
                close(fd);
            }
        }
        if (kind == 1)
            jwrite("unlink", abs, bak, method, NULL);
        else if (kind == 2)
            jwrite("rmlink", abs, lnk, NULL);
        else if (kind == 3)
            jwrite("lost", abs, "unlink", NULL);
    } else if (kind == 1) {
        unlink(bak);
    }
}
```

Update both callers -- `unlink` and `unlinkat` -- to declare
`char foreign_sid[PATH_MAX];` alongside their existing buffers and pass it to
both halves.

`in_our_store` becomes unused once this lands. Delete it rather than leaving a
second, subtly different path matcher for someone to call by mistake.

- [ ] **Step 4: Run the suite**

Run: `test/in-container.sh make test`

Expected: all cases pass, including 31, and 27 (`rm -rf over the store still
undoes`) must still pass — it exercises the *own-store* branch, which the
dispatch above must not have broken.

- [ ] **Step 5: Confirm the floor**

Run the `objdump` command from Task 1 Step 3. Expected: `GLIBC_2.34`. No new
libc calls: `open`, `close`, `opendir`, `readdir`, `closedir` are all already
used elsewhere in the shim.

- [ ] **Step 6: Prove the user's command is unaffected**

The new code runs inside `unlink`. Confirm it changed nothing observable:

```bash
test/in-container.sh --privileged bash -c 'make >/dev/null 2>&1 && test/multifs.sh test/multifs-store.sh'
```

Expected: `store placement ok`, including `an unwritable store never fails the
user's command`.

- [ ] **Step 7: Commit**

```bash
git add shim/undo_shim.c test/e2e.sh
git commit -m 'shim: tell a session when its backups are destroyed by another

A recursive delete of a store root removes every session'"'"'s store under it,
not just the running one. The other sessions kept journals pointing at paths
that no longer existed, counted zero unprotected changes, and reported the
loss as a bare "no such file" partway through a restore -- the one silent
data-loss path left in the tool.

Their backups cannot be evacuated from here: the only directory this process
may copy into is its own session'"'"'s, which they will never read, and charging
one command'"'"'s UNDO_MAX_BYTES for another'"'"'s history is not a trade anyone
asked for. So the loss is recorded rather than prevented -- storemv <path> -
appended to the owning session'"'"'s journal, which its reader already resolves.

undo list now marks those sessions, the pre-revert preview warns, and restore
explains itself. Sessions are siblings under one directory, so $UNDO_SESSION'"'"'s
parent plus the id is the whole derivation; a session that no longer exists is
left to the gc orphan sweep.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes, including new case 31
- [ ] Case 27 (`rm -rf over the store still undoes`) still passes — the own-store branch is intact
- [ ] A session whose store was destroyed by another command shows `(N unprotected)` in `undo list`
- [ ] A backup whose deletion **failed** is not recorded as lost — the record is written only after `rc == 0`
- [ ] No lock, flock, or cross-process synchronization is introduced — see "Known limitation"
- [ ] Its pre-revert preview warns, and restore reports rather than failing on a vanished path
- [ ] `test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-store.sh'` prints `store placement ok`
- [ ] The shim's glibc floor is still `GLIBC_2.34`
- [ ] No session directory is ever created by the shim — a record for a session that no longer exists is dropped
- [ ] A delete of a path containing `/.undo/../` or `/.undo/./` creates no journal anywhere: the id is rejected before any path is built from it
- [ ] `git ls-files -z | xargs -0 tools/check-no-site-data.sh` exits 0

## Deliberately not in this plan

- Evacuating another session's backups. See "Why this and not evacuation".
- GC accounting, `UNDO_MAX_AGE`, and the orphan sweep (2c). The sweep is what
  reclaims a store whose session is gone entirely, which is the case this plan
  deliberately drops.

## Notes for the implementer

- **Never create a session directory.** `other_journal_fd` returns -1 unless the
  directory already exists and is ours. Creating one would resurrect a session
  the user purged, and `undo list` would show a session with a journal and no
  command.
- **Nothing may be recorded before the syscall runs.** The loss record goes in
  `handle_unlink_post`, gated on `rc == 0`. Writing it in the pre-hook asserts a
  loss that has not happened, and a failed unlink then makes an intact backup
  permanently unreachable — discarding recoverable data to report a loss that
  never occurred.
- **The session id comes out of a path the shim was handed, so validate it
  before building anything from it.** `abs_path` does no normalization: without
  `valid_session_id`, `rm <anything>/.undo/../x` extracts ".." as the id, and
  `own_real_dir` happily accepts the resulting `<sessions-dir>/..` because the
  kernel resolves it to a real directory the user owns. An ordinary delete would
  then create a journal one level outside the sessions directory.
- The write happens inside an interposed `unlink`, so `errno` matters. The
  callers of `handle_unlink_pre` already capture and restore it; do not add a
  path that returns early without going through them.
