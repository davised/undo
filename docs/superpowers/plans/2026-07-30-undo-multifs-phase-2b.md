# undo multi-filesystem, Phase 2b — filesystem-local stores and save-method recording

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put each backup on the same filesystem as the file it protects, so a
deletion is a free hardlink instead of a capped copy, and record how each backup
was saved so the collector can tell a free one from an expensive one.

**Architecture:** The shim resolves a store root at save time by walking up from
the file to the highest ancestor that is on the same filesystem, owned by the
caller, and writable, then places backups in `<root>/.undo/<session-id>/`. Three
things guard the consequences: a containment check so the store is never inside
the tree being destroyed, an evacuation path so the backups get off the volume
before the store is destroyed, and a `storemv` journal record so the CLI can
follow a backup that moved. Every backup-bearing journal record gains a trailing
method field.

**Tech Stack:** C (the `LD_PRELOAD` shim, glibc target 2.34), Go 1.24 (journal
reader and restore), bash (e2e and the two-filesystem harness).

## Global Constraints

Copied verbatim from `docs/design/undo-multifs-design.md` and `AGENTS.md`. Every
task inherits these.

- **The shim must never cause the user's command to fail.** All internal errors
  are swallowed; the real syscall's result is returned untouched. Every new
  failure path in this plan is a new opportunity to violate it, so it is
  asserted by test.
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`. The
  build target is el9/x86_64. Verify with `objdump` after every shim change.
  `.github/workflows/ci.yml:54-63` already enforces this and fails the build on
  anything above `GLIBC_2.34` — do not add a second check, but do not rely on CI
  either: a floor regression found locally costs a minute, and found in review
  costs a round trip.
- **No site-specific data in this repository** — no hostnames, mount points,
  organization domains, internal addresses, usernames, or storage capacities.
- **Upstream `CONTRIBUTING.md`:** "Anything touching the shim
  (`shim/undo_shim.c`) should come with an e2e case. The shim runs inside every
  process a user launches: no output on the happy path, no aborts, fail open
  (let the real call through) when anything goes wrong."
- **The journal format is append-only and additive.** Fields may be appended to
  an op, never inserted or reordered. Records must never be filtered out of the
  parsed entry list — `restore.slot()` and `--only` are keyed by position.
- Every build and test runs through `test/in-container.sh`; `test/multifs.sh`
  also needs `--privileged`.

## Environment note

The shim is Linux-only and the workstation is macOS. `test/multifs.sh` mounts
two tmpfs filesystems to obtain distinct `st_dev` without disk images; every
placement assertion in this plan runs inside it.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `shim/undo_shim.c` | modify | path helpers, store resolver + cache, store creation, method recording, store evacuation |
| `internal/journal/journal.go` | modify | `OpStoreMove` constant, `Method()` accessor, `ResolveStoreMoves` |
| `internal/journal/journal_test.go` | modify (append) | method parsing, prefix rewriting, chained and discarded moves |
| `internal/session/session.go` | modify | `load` applies `ResolveStoreMoves` |
| `internal/restore/restore.go` | modify | treat a `-` backup as discarded rather than chasing it; ignore `storemv` records |
| `test/e2e.sh` | modify (append cases 25-27) | the contract cases upstream requires |
| `test/multifs-store.sh` | create | placement, containment, and evacuation on two real filesystems |

## Journal format changes

Both are additive. `journal.Read` already keeps unknown ops and tolerates
trailing fields, so an older CLI reading a newer journal degrades rather than
breaking.

| Op | Before | After |
|---|---|---|
| `unlink` | `path`, `backup` | `path`, `backup`, **`method`** |
| `mod` | `path`, `backup` | `path`, `backup`, **`method`** |
| `rename` | `old`, `new`, `backup`\|`-` | `old`, `new`, `backup`\|`-`, **`method`** |
| `storemv` | — | **new**: `old-path`, `new-path`\|`-` |

`method` is one of `link` (hardlink — allocates nothing), `copy` (full byte
copy), or `none` (no backup was taken). `reflink` is reserved for phase 3. A
record with no method field at all is read as `copy`, which is exactly today's
accounting, so existing journals keep their current behavior.

`storemv` is written **per backup**, not per store. A backup over
`UNDO_MAX_BYTES` cannot be copied off the volume, and a single per-store prefix
record would then point every backup at a location where some of them are not.
A destination of `-` means that backup could not be saved and is gone; it is
rewritten to `-` so restore reports it rather than chasing a missing path.

The resolver matches on `/` component boundaries, so an exact per-file path and
a directory prefix both work through the same code.

## Interfaces produced by this plan

```c
/* shim/undo_shim.c */
static int  path_within(const char *anc, const char *path);   /* anc == path, or path under anc */
static int  has_dotdot(const char *p);
static int  resolve_store_root(const char *abs, char *out);   /* 0 on success */
static int  ensure_store(const char *root, char *out);        /* creates <root>/.undo/<sid> */
static int  backup_name(const char *abs, char *out);          /* was: backup_name(char *out) */
static int  save_file(const char *abs, int need_copy, char *bak, const char **method);
static int  in_our_store(const char *abs, char *root_out);    /* 1 if abs is our backup */
static void evacuate_store(const char *root, int remove_after);
static int  dir_holds_only(const char *dir, const char *name);
```

```go
// internal/journal
const OpStoreMove = "storemv"
func (e Entry) Method() string                  // "link" | "copy" | "none"; "copy" when absent
func ResolveStoreMoves(entries []Entry) []Entry // rewrites backup fields in place-order
```

Plan 2c consumes `Method()` for budget accounting and the `<root>/.undo/<id>/`
layout for the orphan sweep.

---

### Task 1: Path predicates

Two small pure functions the rest of the plan rests on. `abs_path`
(`undo_shim.c:122`) does no normalization — it concatenates the cwd or a
`/proc/self/fd` link with the given path — so `..` components and doubled
separators survive into every path the shim handles. A prefix comparison over
those strings is not a containment test, which is why these are separate,
separately tested functions rather than an inline `strncmp`.

**Files:**
- Modify: `shim/undo_shim.c` (insert after `abs_path`, around line 147)
- Test: `test/multifs-store.sh` is created in Task 6; this task tests through a temporary C harness

**Interfaces:**
- Consumes: nothing (first task)
- Produces: `path_within(const char *anc, const char *path)`, `has_dotdot(const char *p)`

- [ ] **Step 1: Write the failing test harness**

Create `test/pathpred.c` — a throwaway harness that includes the shim's source
so it can reach the static functions:

```c
/* Unit harness for the shim's path predicates. Includes the translation unit
 * directly, which is how static functions get tested without exporting them. */
#include "../shim/undo_shim.c"
#include <stdio.h>

static int failures;

static void expect(int got, int want, const char *what)
{
    if (got != want) {
        printf("FAIL: %s -> %d, want %d\n", what, got, want);
        failures++;
    }
}

int main(void)
{
    /* the boundary case a naive prefix test gets wrong */
    expect(path_within("/x/a", "/x/ab"), 0, "path_within(/x/a, /x/ab)");
    expect(path_within("/x/a", "/x/a"), 1, "path_within(/x/a, /x/a)");
    expect(path_within("/x/a", "/x/a/b"), 1, "path_within(/x/a, /x/a/b)");
    expect(path_within("/x/ab", "/x/a"), 0, "path_within(/x/ab, /x/a)");
    expect(path_within("/", "/anything"), 1, "path_within(/, /anything)");
    expect(path_within("/x/a/", "/x/a/b"), 1, "trailing slash tolerated");
    expect(path_within("/x/b", "/x/a/b"), 0, "path_within(/x/b, /x/a/b)");

    expect(has_dotdot("/x/../y"), 1, "has_dotdot(/x/../y)");
    expect(has_dotdot("/x/y/.."), 1, "has_dotdot(/x/y/..)");
    expect(has_dotdot("/x/..y"), 0, "has_dotdot(/x/..y)");
    expect(has_dotdot("/x/y..z"), 0, "has_dotdot(/x/y..z)");
    expect(has_dotdot("/x/y"), 0, "has_dotdot(/x/y)");
    expect(has_dotdot("/.."), 1, "has_dotdot leading");
    expect(has_dotdot("/x//../y"), 1, "doubled separators");

    if (failures == 0)
        printf("path predicates ok\n");
    return failures != 0;
}
```

`mkdtemp` and `system` are used by the Task 2 addition to this file, so
`<stdlib.h>` is needed — it already arrives via the included shim source.

- [ ] **Step 2: Run it and watch it fail**

Run:

```bash
test/in-container.sh bash -c 'gcc -o /tmp/pathpred test/pathpred.c -ldl && /tmp/pathpred'
```

Expected: compile failure — `path_within` and `has_dotdot` are undefined.

- [ ] **Step 3: Implement**

In `shim/undo_shim.c`, insert after `abs_path` (after line 147):

```c
/* True when `path` is `anc` itself or lies beneath it, compared on
 * '/'-delimited component boundaries.
 *
 * A bare strncmp would confuse /x/a with /x/ab, and abs_path does no
 * normalization, so these are raw strings that may still carry doubled
 * separators. Callers reject '..' separately with has_dotdot(); this function
 * assumes it has already been done. */
static int path_within(const char *anc, const char *path)
{
    size_t n = strlen(anc);
    while (n > 1 && anc[n - 1] == '/')
        n--;
    if (n == 0)
        return 0;
    if (strncmp(path, anc, n) != 0)
        return 0;
    if (n == 1 && anc[0] == '/')
        return 1; /* everything absolute is under the root */
    return path[n] == '\0' || path[n] == '/';
}

/* True when `p` contains a ".." path component. abs_path does not resolve
 * them, and a candidate carrying one cannot be compared against anything
 * safely, so such candidates are rejected rather than normalized -- realpath()
 * on a network filesystem is too expensive to run on the hot path. */
static int has_dotdot(const char *p)
{
    const char *s = p;
    while (*s) {
        while (*s == '/')
            s++;
        if (s[0] == '.' && s[1] == '.' && (s[2] == '/' || s[2] == '\0'))
            return 1;
        while (*s && *s != '/')
            s++;
    }
    return 0;
}
```

- [ ] **Step 4: Run the harness**

Run:

```bash
test/in-container.sh bash -c 'gcc -o /tmp/pathpred test/pathpred.c -ldl && /tmp/pathpred'
```

Expected: `path predicates ok`, exit 0.

- [ ] **Step 5: Confirm the floor did not move**

Run:

```bash
test/in-container.sh bash -c '
  gcc -shared -fPIC -O2 -Wall -Wextra -o /tmp/libundo.so shim/undo_shim.c -ldl
  objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
```

Expected: `GLIBC_2.34` or lower.

- [ ] **Step 6: Commit**

```bash
git add shim/undo_shim.c test/pathpred.c
git commit -m 'shim: component-boundary path containment predicates

Store placement has to answer "is this candidate inside the tree being
destroyed", and abs_path does no normalization -- it concatenates the cwd or
a /proc/self/fd link with the given path, so .. components and doubled
separators survive. A strncmp over those strings is not a containment test:
it confuses /x/a with /x/ab, and it reads /x/tree/../tree as unrelated to
/x/tree.

path_within compares on / boundaries. has_dotdot rejects the candidates that
cannot be compared at all, rather than calling realpath() -- which on a
network filesystem is a round trip the shim cannot afford on the hot path.
Symlink aliasing therefore stays a documented gap.'
```

---

### Task 2: The store resolver and its cache

Walk up from the file to the highest ancestor that is on its filesystem, owned
by the effective uid, and owner-writable. That ancestor hosts the store.

Ownership rather than mere writability is deliberate: on shared storage the
mount root is typically not user-writable while a directory the user owns
inside it is, so "highest owned" lands on the user's own top-level directory on
that volume. "Highest writable" would risk landing in a shared directory, where
one user's backups sit in space belonging to everyone.

**Files:**
- Modify: `shim/undo_shim.c` (insert after `has_dotdot`)
- Test: `test/pathpred.c` (extend)

**Interfaces:**
- Consumes: `path_within`, `has_dotdot` from Task 1.
- Produces: `resolve_store_root(const char *abs, char *out)` returning 0 and filling `out` on success, -1 otherwise.

- [ ] **Step 1: Write the failing test**

Append to `test/pathpred.c`, before the `if (failures == 0)` line in `main`:

```c
    /* resolver: build a tree we own and check it walks to the top of it */
    {
        char tmpl[] = "/tmp/undo-resolve-XXXXXX";
        char *base = mkdtemp(tmpl);
        if (!base) {
            printf("FAIL: mkdtemp\n");
            failures++;
        } else {
            char deep[PATH_MAX], got[PATH_MAX], file[PATH_MAX];
            snprintf(deep, sizeof deep, "%s/a/b/c", base);
            char mk[PATH_MAX + 32];
            snprintf(mk, sizeof mk, "mkdir -p %s", deep);
            if (system(mk) != 0) {
                printf("FAIL: mkdir -p\n");
                failures++;
            }
            snprintf(file, sizeof file, "%s/f.txt", deep);
            FILE *fp = fopen(file, "w");
            if (fp) fclose(fp);

            /* everything from /tmp down is ours and writable, so the walk
             * should climb past the temp dir; assert only that the answer is
             * an ancestor of the file and on the same filesystem */
            if (resolve_store_root(file, got) != 0) {
                printf("FAIL: resolve_store_root returned -1\n");
                failures++;
            } else {
                expect(path_within(got, file), 1, "resolved root is an ancestor");
                struct stat sr, sf;
                stat(got, &sr);
                stat(file, &sf);
                expect(sr.st_dev == sf.st_dev, 1, "resolved root is on the same fs");
                expect(sr.st_uid == geteuid(), 1, "resolved root is owned by us");
            }
        }
    }
```

- [ ] **Step 2: Run it and watch it fail**

Run:

```bash
test/in-container.sh bash -c 'gcc -o /tmp/pathpred test/pathpred.c -ldl && /tmp/pathpred'
```

Expected: compile failure — `resolve_store_root` is undefined.

- [ ] **Step 3: Implement**

In `shim/undo_shim.c`, insert after `has_dotdot`:

```c
/* ---------- filesystem-local store placement ---------- */

#define STORE_CACHE_SLOTS 8

struct store_cache_ent {
    dev_t dev;
    char root[PATH_MAX];
};

/* Thread-local, like the journal descriptor above. A process-wide table would
 * need a lock, and taking a lock inside an interposer invites deadlock and
 * leaves undefined state across fork(). Per-thread walks cost extra stat()
 * calls; the unsynchronized dedup_tab below is the precedent worth not
 * repeating. */
static __thread struct store_cache_ent store_cache[STORE_CACHE_SLOTS];
static __thread int store_cache_next;

static void store_cache_put(dev_t dev, const char *root)
{
    store_cache[store_cache_next].dev = dev;
    snprintf(store_cache[store_cache_next].root, PATH_MAX, "%s", root);
    store_cache_next = (store_cache_next + 1) % STORE_CACHE_SLOTS;
}

/* Highest ancestor of `abs` on the same filesystem, owned by the effective
 * uid, and writable by its owner. Returns 0 and fills `out` on success.
 *
 * The upward walk costs several stat() calls, which on a network filesystem
 * are round trips, hence the cache. st_dev alone is not a sufficient key: one
 * filesystem can hold several user-owned subtrees beneath different
 * group-owned parents, and whichever resolved first would otherwise hand back
 * a root that is not an ancestor of a later path -- silently invalidating the
 * containment decision. A hit is therefore accepted only when the cached root
 * still contains the current path. */
static int resolve_store_root(const char *abs, char *out)
{
    char cur[PATH_MAX];
    struct stat st;

    if ((size_t)snprintf(cur, sizeof cur, "%s", abs) >= sizeof cur)
        return -1;
    if (has_dotdot(cur))
        return -1;

    /* The file itself may already be gone (rename target, racing unlink), so
     * fall back to its directory for the device number. */
    if (stat(cur, &st) != 0) {
        char *slash = strrchr(cur, '/');
        if (!slash)
            return -1;
        if (slash == cur)
            cur[1] = 0;
        else
            *slash = 0;
        if (stat(cur, &st) != 0)
            return -1;
    }

    dev_t dev = st.st_dev;
    uid_t me = geteuid();

    for (int i = 0; i < STORE_CACHE_SLOTS; i++) {
        if (store_cache[i].root[0] && store_cache[i].dev == dev &&
            path_within(store_cache[i].root, abs)) {
            snprintf(out, PATH_MAX, "%s", store_cache[i].root);
            return 0;
        }
    }

    snprintf(cur, sizeof cur, "%s", abs);
    char best[PATH_MAX];
    best[0] = 0;
    for (;;) {
        char *slash = strrchr(cur, '/');
        if (!slash)
            break;
        if (slash == cur)
            cur[1] = 0; /* the root directory */
        else
            *slash = 0;
        if (stat(cur, &st) != 0 || st.st_dev != dev)
            break; /* left the filesystem, or cannot see it */
        if (st.st_uid == me && (st.st_mode & S_IWUSR))
            snprintf(best, sizeof best, "%s", cur);
        if (cur[0] == '/' && cur[1] == 0)
            break;
    }

    if (!best[0])
        return -1;
    snprintf(out, PATH_MAX, "%s", best);
    store_cache_put(dev, best);
    return 0;
}
```

The walk starts by stripping the last component, so a candidate is always a
strict ancestor of `abs` and never `abs` itself.

- [ ] **Step 4: Run the harness**

Run:

```bash
test/in-container.sh bash -c 'gcc -o /tmp/pathpred test/pathpred.c -ldl && /tmp/pathpred'
```

Expected: `path predicates ok`.

- [ ] **Step 5: Confirm the floor**

Run the `objdump` command from Task 1 Step 5. Expected: `GLIBC_2.34` or lower.
`stat`, `geteuid`, and `strrchr` are all long-standing symbols; if this moved,
something else was added by mistake.

- [ ] **Step 6: Commit**

```bash
git add shim/undo_shim.c test/pathpred.c
git commit -m 'shim: resolve a store root per filesystem at runtime

link(2) returns EXDEV across mounts, so a single store degrades every file
on another filesystem to a size-capped copy -- and above the cap, to nothing
at all. On a host with per-lab mounts that is the common case, not the edge.

The root is the highest ancestor on the file'"'"'s own filesystem that the
caller owns and can write. Owned rather than merely writable: on shared
storage the mount root is usually not user-writable while a directory the
user owns inside it is, so "highest owned" lands on the user'"'"'s own top-level
directory. "Highest writable" could land in a shared directory, putting one
user'"'"'s backups in everyone'"'"'s space.

Resolution is at runtime rather than from configuration because st_dev is
assigned at mount time and differs between hosts, mount sets differ too, and
mounts come and go. No static site map can be correct everywhere -- and a
runtime walk encodes nothing site-specific in the source.

The cache is thread-local, like the journal descriptor: a process-wide table
needs a lock, and locking inside an interposer invites deadlock and leaves
undefined state across fork(). A hit requires the cached root to still
contain the path, since one filesystem can hold several owned subtrees.'
```

---

### Task 3: Place backups in the store, and record how they were saved

`backup_name` currently returns `$UNDO_SESSION/data/<pid>-<n>` unconditionally.
It becomes store-aware, and `save_file` reports which mechanism succeeded.

The save method cannot be recovered afterwards. The shim hardlinks the file and
the real `unlink` immediately removes the original, so the link count is back to
1 before anything inspects it — a hardlinked backup is indistinguishable from a
copy by the time GC runs. `test/multifs.sh` asserts exactly this. So it is
recorded at save time or not at all.

**Files:**
- Modify: `shim/undo_shim.c` — `backup_name` (181-192), `save_file` (245-269), the three `handle_*` pairs (401-542)
- Test: `test/e2e.sh` (append case 25)

**Interfaces:**
- Consumes: `resolve_store_root`, `path_within`, `has_dotdot` from Tasks 1-2.
- Produces: `ensure_store`, the new `backup_name(const char *abs, char *out)`, `save_file(..., const char **method)`, and journal records carrying a trailing method field.

- [ ] **Step 1: Write the failing e2e case**

Append to `test/e2e.sh`, after case 24 and before the closing `echo "all cases
passed"`:

```bash
echo "== case 25: a deletion is hardlinked into a store beside the file"
mkdir -p "$PLAY/store-local"
echo "large enough to matter" >"$PLAY/store-local/big.bin"
run_armed "rm $PLAY/store-local/big.bin"
sess=$(ls -1d "$UNDO_DATA_DIR"/sessions/* | tail -1)
grep -q $'\tlink$' "$sess/journal" ||
    fail "the unlink record does not end with the save method"
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | tail -1)
case $bak in
    */.undo/*) ;;
    *) fail "backup landed at $bak, not in a .undo store" ;;
esac
[[ -f $bak ]] || fail "the backup file is missing"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/store-local/big.bin") == "large enough to matter" ]] ||
    fail "restore from a filesystem-local store failed"
```

- [ ] **Step 2: Run it and watch it fail**

Run: `test/in-container.sh test/e2e.sh`

Expected: `FAIL: the unlink record does not end with the save method`. If it
fails earlier on the store path instead, that is also correct — both properties
are missing.

- [ ] **Step 3: Add store creation**

In `shim/undo_shim.c`, insert after `resolve_store_root`:

```c
/* The session id is the basename of $UNDO_SESSION. */
static const char *session_id(void)
{
    const char *dir = session_dir();
    if (!dir)
        return NULL;
    const char *slash = strrchr(dir, '/');
    return (slash && slash[1]) ? slash + 1 : NULL;
}

/* Create <root>/.undo/<session-id>/ and return it in `out`.
 *
 * real_mkdir rather than mkdir(): mkdir is itself interposed, and creating a
 * store directory must never be journaled as user activity. Callers already
 * hold in_shim, which would suppress it anyway; going through real_mkdir means
 * this does not silently depend on that. */
static int ensure_store(const char *root, char *out)
{
    const char *sid = session_id();
    if (!sid)
        return -1;
    char undo[PATH_MAX];
    if ((size_t)snprintf(undo, sizeof undo, "%s/.undo", root) >= sizeof undo)
        return -1;
    REAL(mkdir, int, const char *, mode_t);
    if (real_mkdir(undo, 0700) != 0 && errno != EEXIST)
        return -1;
    if ((size_t)snprintf(out, PATH_MAX, "%s/%s", undo, sid) >= PATH_MAX)
        return -1;
    if (real_mkdir(out, 0700) != 0 && errno != EEXIST)
        return -1;
    return 0;
}
```

- [ ] **Step 4: Make `backup_name` store-aware**

Replace `backup_name` (lines 181-192) with:

```c
/* Where to put the backup of `abs`.
 *
 * Prefers a store on abs's own filesystem so a deletion can be a free
 * hardlink; falls back to the session directory, which always exists but may
 * be on another filesystem and therefore capped by UNDO_MAX_BYTES.
 *
 * Containment: a store inside the tree being modified would put backups in
 * the directory they exist to protect, so a recursive delete would race its
 * own safety net. The check is applied to the fallback too, since
 * $UNDO_SESSION may itself lie beneath the operated tree. */
static int backup_name(const char *abs, char *out)
{
    static unsigned long counter;
    const char *dir = session_dir();
    char store[PATH_MAX], root[PATH_MAX];
    int have = 0;

    if (!dir)
        return -1;

    if (!has_dotdot(abs) && resolve_store_root(abs, root) == 0 &&
        !has_dotdot(root) && !path_within(abs, root) &&
        ensure_store(root, store) == 0)
        have = 1;

    if (!have) {
        if ((size_t)snprintf(store, sizeof store, "%s/data", dir) >= sizeof store)
            return -1;
        if (has_dotdot(abs) || path_within(abs, store))
            return -1; /* the fallback is inside the tree being operated on */
    }

    unsigned long n = __atomic_add_fetch(&counter, 1, __ATOMIC_RELAXED);
    if ((size_t)snprintf(out, PATH_MAX, "%s/%d-%lu", store, (int)getpid(), n) >=
        PATH_MAX)
        return -1;
    return 0;
}
```

- [ ] **Step 5: Make `save_file` report the method**

Replace `save_file` (lines 245-269) with:

```c
/* Save `abs` before it is destroyed, and report how.
 *
 * The method cannot be recovered later: the shim hardlinks the file and the
 * real unlink immediately removes the original, so the link count is back to
 * 1 long before anything inspects the backup. A hardlink and a copy are
 * indistinguishable by then, which is why the collector is told here instead
 * of guessing from st_nlink. */
static int save_file(const char *abs, int need_copy, char *bak,
                     const char **method)
{
    *method = "none";
    /* Names can collide when a shell execs its last command without forking
     * (same pid, counter reset); retry with the next counter. */
    for (int tries = 0; tries < 1000; tries++) {
        if (backup_name(abs, bak) != 0)
            return -1;
        if (!need_copy) {
            REAL(link, int, const char *, const char *);
            if (real_link(abs, bak) == 0) {
                *method = "link";
                return 0;
            }
            if (errno == EEXIST)
                continue;
            /* cross-device etc: fall through to a copy under this name */
        }
        if (copy_file(abs, bak) == 0) {
            *method = "copy";
            return 0;
        }
        if (errno != EEXIST)
            break;
    }
    if (getenv("UNDO_DEBUG"))
        fprintf(stderr, "undo-shim: could not save %s: %s\n", abs,
                strerror(errno));
    return -1;
}
```

- [ ] **Step 6: Thread the method through the handlers**

Three `handle_*_pre` functions call `save_file`, and their matching `_post`
functions write the journal record. Each gains a `const char **method`
parameter, and each interposed caller declares `const char *method = "none";`
alongside its existing buffers.

`handle_unlink_pre` / `handle_unlink_post`:

```c
static void handle_unlink_pre(int dirfd, const char *path, char *abs,
                              char *bak, char *lnk, int *kind,
                              const char **method)
{
    *kind = 0;
    *method = "none";
    if (abs_path(dirfd, path, abs) != 0 || ignored(abs))
        return;
    struct stat st;
    if (lstat(abs, &st) != 0)
        return;
    if (S_ISLNK(st.st_mode)) {
        ssize_t n = readlink(abs, lnk, PATH_MAX - 1);
        if (n >= 0) {
            lnk[n] = 0;
            *kind = 2;
        }
        return;
    }
    if (S_ISREG(st.st_mode)) {
        if (save_file(abs, 0, bak, method) == 0)
            *kind = 1;
        else
            *kind = 3; /* existed but could not be saved */
    }
}

static void handle_unlink_post(int rc, const char *abs, const char *bak,
                               const char *lnk, int kind, const char *method)
{
    if (rc == 0) {
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

`handle_open_pre` gains the same parameter, passing it to
`save_file(abs, 1, bak, method)`, and `handle_open_post` writes
`jwrite("mod", abs, bak, method, NULL)` for `kind == 1`.

`handle_rename_pre` gains it, passing to `save_file(absnew, 0, bak, method)`,
and `handle_rename_post` writes:

```c
        if (kind == 1)
            jwrite("rename", absold, absnew, "-", "none", NULL);
        else if (kind == 2)
            jwrite("rename", absold, absnew, bak, method, NULL);
```

The `"none"` on the no-backup arm keeps the method at a fixed field position
for every `rename` record, so a reader never has to count fields to find it.

Update all seven call sites: `unlink` (546), `unlinkat` (577), `do_rename`
(611), `truncate_common` (814), `open_common` (858), `fopen_common` (971), and
`freopen` (998). `truncate_common` calls `save_file` directly and writes its own
`mod` record — it needs a local `const char *method = "none";` too.

- [ ] **Step 7: Run the suite**

Run: `test/in-container.sh make test`

Expected: `go test ./...` passes and every e2e case passes including the new
case 25. Compiler warnings are errors of judgment here — the build uses `-Wall
-Wextra`, and an unused-parameter warning means a handler was missed.

- [ ] **Step 8: Confirm the floor**

Run the `objdump` command from Task 1 Step 5. Expected: `GLIBC_2.34` or lower.

- [ ] **Step 9: Commit**

```bash
git add shim/undo_shim.c test/e2e.sh
git commit -m 'shim: place backups on the file'"'"'s own filesystem and record the method

Backups went to $UNDO_SESSION/data unconditionally, so on a multi-filesystem
host link(2) returned EXDEV for anything not on the store'"'"'s filesystem and
every deletion degraded to a size-capped copy -- above the cap, to nothing at
all. Backups now go to <root>/.undo/<session-id>/ on the filesystem of the
file being saved, where the hardlink works and costs nothing.

The save method is recorded because it cannot be recovered afterwards: the
shim hardlinks the file and the real unlink immediately removes the original,
so the link count is back to 1 before anything inspects the backup, and a
hardlink is indistinguishable from a copy by then. Without the field, the
collector counts a free 50 GiB hardlink against a 1 GiB budget and evicts it
on the next command -- defeating the mechanism precisely for the large files
it exists to protect.

The field is appended, and journal.Read already tolerates trailing fields,
so an older CLI reads these journals unchanged.'
```

---

### Task 4: Get the backups out before the store is destroyed

Two ways the store can be taken out from under itself, both reachable on a
volume where per-user directories sit beneath a group-owned parent — the walk
terminates inside the user's tree, so the store lands there:

1. **`rmdir` fails `ENOTEMPTY` because our `.undo/` is the last entry.** The
   user's command fails because undo was loaded, which is the one thing the
   shim must never do.
2. **`rm -rf` of an ancestor deletes the store outright**, backups and all.
   Confirmed by reproduction: a `.undo` directory nested under a removed
   ancestor goes with the tree. This is the larger of the two, and it is silent.

**There is nowhere better on that filesystem to put them.** The store root is
the *highest* owned-and-writable ancestor — the walk climbs to the mount
boundary and overwrites its candidate each time — so by construction nothing
above the store root is both owned and writable on that device. Moving the
store up one level therefore cannot work: the parent never passes the test that
chose the root in the first place. An earlier revision of this plan tried it and
was dead code.

So the backups are **copied to the session directory**, which is off-volume and
outside any tree the command is deleting, subject to `UNDO_MAX_BYTES` per file.
Anything over the cap, or that fails to copy, gets a `storemv <old> -` record so
the loss is reported rather than silent. Copied rather than moved: on the
`rm -rf` path the originals must stay for `rm` to delete, or `rm` reports
missing files and the exit status changes.

This is the one place the never-fail invariant genuinely costs something — a
hardlinked backup becomes real bytes on the session filesystem. It is bounded by
the cap, and it only happens when the user is deleting their own top-level
directory on a volume.

**Files:**
- Modify: `shim/undo_shim.c` — `ignored` (297-335), `handle_unlink_pre` (401-424), `rmdir` (561-575), `unlinkat` (577-598)
- Test: `test/e2e.sh` (append cases 26 and 27)

**Interfaces:**
- Consumes: `session_id`, `ensure_store`, `copy_file`, `jwrite` from earlier tasks.
- Produces: `in_our_store(const char *abs, char *root_out)`, `evacuate_store(const char *root, int remove_after)`, and per-backup `storemv` records.

- [ ] **Step 1: Write the failing e2e cases**

Append to `test/e2e.sh`, after case 25:

```bash
echo "== case 26: our own store never makes an rmdir fail"
mkdir -p "$PLAY/relocate/inner"
echo "recoverable" >"$PLAY/relocate/inner/doomed.txt"
run_armed "rm $PLAY/relocate/inner/doomed.txt && rmdir $PLAY/relocate/inner"
[[ ! -e $PLAY/relocate/inner ]] ||
    fail "rmdir left the directory behind; the shim broke the command"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/relocate/inner/doomed.txt") == "recoverable" ]] ||
    fail "the backup did not survive the store evacuation"

echo "== case 27: rm -rf over the store still undoes"
mkdir -p "$PLAY/wipe/sub"
echo "keep me" >"$PLAY/wipe/sub/a.txt"
echo "me too" >"$PLAY/wipe/b.txt"
run_armed "rm -rf $PLAY/wipe"
[[ ! -e $PLAY/wipe ]] || fail "rm -rf did not run"
"$UNDO" -y >/dev/null
[[ $(cat "$PLAY/wipe/sub/a.txt") == "keep me" ]] ||
    fail "a backup inside the destroyed store was not evacuated"
[[ $(cat "$PLAY/wipe/b.txt") == "me too" ]] ||
    fail "a backup inside the destroyed store was not evacuated"
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh test/e2e.sh`

Expected: FAIL on one or both. Which assertion fires depends on where the walk
lands in the container's temp directory. If **both pass already**, the store did
not land inside the removed tree and the cases are proving nothing — check with
`awk -F'\t' '$1=="unlink"{print $3}'` on the session journal, and adjust the
fixture so the store root really is inside the tree before continuing.

- [ ] **Step 3: Stop the shim journaling its own store**

`rm -rf` walking into `.undo/` currently makes the shim back up each backup
into the same doomed store. In `shim/undo_shim.c`, add an unconditional check at
the top of `ignored` (line 297), before the `use_default` block:

```c
static int ignored(const char *abs)
{
    /* Our own store, always. Not part of default_ignores, which
     * UNDO_DEFAULT_IGNORE=0 turns off: backing up our own backups is never
     * something a user should be able to switch on. */
    if (seg_match(abs, ".undo", 5))
        return 1;

    static int loaded, use_default = 1;
    ...
```

- [ ] **Step 4: Implement the evacuation**

Insert after `ensure_store`:

```c
/* If `abs` is a backup inside this session's own store, fill `root_out` with
 * that store's root and return 1.
 *
 * Backups live at <root>/.undo/<session-id>/<name>, so this is a structural
 * match on the two components before the basename. */
static int in_our_store(const char *abs, char *root_out)
{
    const char *sid = session_id();
    if (!sid)
        return 0;
    size_t sidlen = strlen(sid);

    const char *p = abs;
    while ((p = strstr(p, "/.undo/")) != NULL) {
        const char *after = p + 7; /* strlen("/.undo/") */
        if (strncmp(after, sid, sidlen) == 0 && after[sidlen] == '/') {
            size_t rootlen = (size_t)(p - abs);
            if (rootlen == 0 || rootlen >= PATH_MAX)
                return 0;
            memcpy(root_out, abs, rootlen);
            root_out[rootlen] = 0;
            return 1;
        }
        p += 7;
    }
    return 0;
}

/* Roots already evacuated by this thread, so a recursive delete does not
 * re-copy the whole store once per file it removes. */
#define EVAC_SLOTS 8
static __thread char evac_done[EVAC_SLOTS][PATH_MAX];
static __thread int evac_next;

static int evac_seen(const char *root)
{
    for (int i = 0; i < EVAC_SLOTS; i++)
        if (evac_done[i][0] && strcmp(evac_done[i], root) == 0)
            return 1;
    snprintf(evac_done[evac_next], PATH_MAX, "%s", root);
    evac_next = (evac_next + 1) % EVAC_SLOTS;
    return 0;
}

/* Copy every backup in <root>/.undo/<session-id>/ to the session directory,
 * recording where each one went, because the store is about to be destroyed.
 *
 * Copied, not moved: on the rm -rf path the originals must stay for rm to
 * delete. Moving them makes rm report missing files and changes the command's
 * exit status, which is exactly what the shim must never do.
 *
 * One storemv record per backup rather than one prefix record for the store:
 * a file over UNDO_MAX_BYTES cannot be copied, and a prefix record would then
 * point every backup at a location where some of them are not. Per-file, a
 * failure is recorded as "-" and reported honestly.
 *
 * With remove_after set the originals are removed and the store directories
 * taken down, which is what lets a failing rmdir be retried.
 */
static void evacuate_store(const char *root, int remove_after)
{
    const char *dir = session_dir();
    const char *sid = session_id();
    if (!dir || !sid)
        return;
    if (!remove_after && evac_seen(root))
        return;

    char store[PATH_MAX];
    if ((size_t)snprintf(store, sizeof store, "%s/.undo/%s", root, sid) >=
        sizeof store)
        return;

    DIR *d = opendir(store);
    if (!d)
        return;
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0)
            continue;
        char from[PATH_MAX], to[PATH_MAX];
        if ((size_t)snprintf(from, sizeof from, "%s/%s", store, e->d_name) >=
                sizeof from ||
            (size_t)snprintf(to, sizeof to, "%s/data/%s", dir, e->d_name) >=
                sizeof to)
            continue;
        /* copy_file enforces UNDO_MAX_BYTES and refuses non-regular files */
        if (copy_file(from, to) == 0)
            jwrite("storemv", from, to, NULL);
        else
            jwrite("storemv", from, "-", NULL);
        if (remove_after) {
            REAL(unlink, int, const char *);
            real_unlink(from);
        }
    }
    closedir(d);

    if (remove_after) {
        char undo[PATH_MAX];
        REAL(rmdir, int, const char *);
        real_rmdir(store);
        if ((size_t)snprintf(undo, sizeof undo, "%s/.undo", root) < sizeof undo)
            real_rmdir(undo);
    }
}
```

Add `#include <dirent.h>` to the include block at the top of the file.

- [ ] **Step 5: Trigger it when a backup of ours is being deleted**

In `handle_unlink_pre`, immediately after `abs_path` succeeds and **before** the
`ignored` check — `.undo` is now always ignored, so the check would otherwise
return first:

```c
static void handle_unlink_pre(int dirfd, const char *path, char *abs,
                              char *bak, char *lnk, int *kind,
                              const char **method)
{
    *kind = 0;
    *method = "none";
    if (abs_path(dirfd, path, abs) != 0)
        return;
    /* Something is deleting our own backups -- a recursive delete that reached
     * the store. Get them off this filesystem before they go. */
    char root[PATH_MAX];
    if (in_our_store(abs, root))
        evacuate_store(root, 0);
    if (ignored(abs))
        return;
    ...
```

- [ ] **Step 6: Trigger it when our store blocks an `rmdir`**

Add a helper beside `evacuate_store`:

```c
/* True when `dir` contains exactly one entry and it is named `name`. */
static int dir_holds_only(const char *dir, const char *name)
{
    DIR *d = opendir(dir);
    if (!d)
        return 0;
    int seen = 0, other = 0;
    struct dirent *e;
    while ((e = readdir(d)) != NULL) {
        if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0)
            continue;
        if (strcmp(e->d_name, name) == 0)
            seen = 1;
        else
            other = 1;
    }
    closedir(d);
    return seen && !other;
}
```

Replace `rmdir` (561-575):

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
    int saved = errno;
    if (rc != 0 && saved == ENOTEMPTY && ok && dir_holds_only(abs, ".undo")) {
        evacuate_store(abs, 1);
        rc = real_rmdir(path);
        saved = errno;
    }
    if (rc == 0 && ok)
        jwrite("rmdir", abs, mode, NULL);
    in_shim = 0;
    errno = saved; /* opendir, copy, unlink and rmdir all clobber it */
    return rc;
}
```

In `unlinkat` (577-598), apply the same shape in the `AT_REMOVEDIR` branch, and
capture `errno` around the whole thing:

```c
    int rc = real_unlinkat(dirfd, path, flags);
    int saved = errno;
    if (flags & AT_REMOVEDIR) {
        if (rc != 0 && saved == ENOTEMPTY && dirok &&
            dir_holds_only(abs, ".undo")) {
            evacuate_store(abs, 1);
            rc = real_unlinkat(dirfd, path, flags);
            saved = errno;
        }
        if (rc == 0 && dirok)
            jwrite("rmdir", abs, mode, NULL);
    } else {
        handle_unlink_post(rc, abs, bak, lnk, kind, method);
    }
    in_shim = 0;
    errno = saved;
    return rc;
```

**`errno` matters as much as the return value here.** The retry path runs
`opendir`, `copy_file`, `unlink`, and `rmdir` between the failing call and the
caller's return. A caller whose `rmdir` genuinely failed must still see
`ENOTEMPTY`, not whatever the last internal syscall left behind.

- [ ] **Step 7: Run the suite**

Run: `test/in-container.sh make test`

Expected: all cases pass, including 25, 26, and 27.

- [ ] **Step 8: Confirm the floor**

Run the `objdump` command from Task 1 Step 5. The new symbols are `opendir`,
`readdir`, and `closedir`, all `GLIBC_2.2.5`. Expected: `GLIBC_2.34` or lower.
**If `readdir` resolved to a variant that raised the floor, stop** — that is
precisely the regression class the constraint exists for.

- [ ] **Step 9: Commit**

```bash
git add shim/undo_shim.c test/e2e.sh
git commit -m 'shim: evacuate backups before their own store is destroyed

Two ways the store gets taken out from under itself on a volume where
per-user directories sit under a group-owned parent, which is where the walk
terminates inside the user tree:

  - rmdir fails ENOTEMPTY because our .undo is the last entry left, so the
    user command fails because undo was loaded;
  - rm -rf of an ancestor deletes the store outright, silently, backups and
    all.

There is nowhere better on that filesystem to move them. The store root is
the HIGHEST owned and writable ancestor, so by construction nothing above it
is both owned and writable -- moving the store up one level cannot work,
because the parent never passes the test that chose the root.

So the backups are copied to the session directory, which is off-volume and
outside any tree being deleted, capped by UNDO_MAX_BYTES. Copied rather than
moved: on the rm -rf path the originals have to stay for rm to delete, or rm
reports missing files and the exit status changes.

One storemv record per backup, not one prefix record per store: a file over
the cap cannot be copied, and a prefix record would then point every backup
at a location where some of them are not. Per-file, a failure is recorded as
"-" and reported instead of discovered during a restore.

.undo is now ignored unconditionally rather than via default_ignores, which
UNDO_DEFAULT_IGNORE=0 disables. Backing up our own backups is not something
a user should be able to switch on.

errno is captured across the retry: opendir, copy_file, unlink and rmdir all
clobber it, and a caller whose rmdir genuinely failed must still see why.'
```

---

### Task 5: Teach the CLI the method field and `storemv`

The shim now writes records the Go side does not understand. `journal.Read`
keeps unknown ops, so nothing breaks — but backups that moved are unreachable
until the reader follows them, and `restore` reports a raw `ENOENT` for a
discarded one instead of saying what happened.

**Files:**
- Modify: `internal/journal/journal.go`
- Modify: `internal/session/session.go` — `load` (61-92)
- Modify: `internal/restore/restore.go` — `OpUnlink` and `OpMod` cases, plus a `storemv` no-op arm
- Test: `internal/journal/journal_test.go` (append)

**Interfaces:**
- Consumes: the record formats from Tasks 3-4.
- Produces: `journal.OpStoreMove`, `(Entry).Method() string`, `journal.ResolveStoreMoves([]Entry) []Entry`. Plan 2c consumes `Method()`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/journal/journal_test.go`:

```go
func TestMethodDefaultsToCopy(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want string
	}{
		{"unlink hardlinked", Entry{Op: OpUnlink, Fields: []string{"/f", "/b", "link"}}, "link"},
		{"mod copied", Entry{Op: OpMod, Fields: []string{"/f", "/b", "copy"}}, "copy"},
		{"rename with backup", Entry{Op: OpRename, Fields: []string{"/a", "/b", "/bak", "link"}}, "link"},
		{"rename without", Entry{Op: OpRename, Fields: []string{"/a", "/b", "-", "none"}}, "none"},
		// A journal written before the field existed must keep its old
		// accounting, which counted everything at full size.
		{"legacy unlink", Entry{Op: OpUnlink, Fields: []string{"/f", "/b"}}, "copy"},
		{"no backup at all", Entry{Op: OpCreate, Fields: []string{"/f"}}, "none"},
	}
	for _, c := range cases {
		if got := c.e.Method(); got != c.want {
			t.Errorf("%s: Method() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResolveStoreMovesRewritesLaterOnly(t *testing.T) {
	in := []Entry{
		{Op: OpUnlink, Fields: []string{"/v/p/a.txt", "/v/p/.undo/S/1-1", "link"}},
		{Op: OpStoreMove, Fields: []string{"/v/p/.undo/S", "/v/.undo/S"}},
		// taken after the move: already correct, must not be rewritten twice
		{Op: OpUnlink, Fields: []string{"/v/p/b.txt", "/v/.undo/S/1-2", "link"}},
	}
	out := ResolveStoreMoves(in)
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d: indices are load-bearing", len(out), len(in))
	}
	if got := out[0].Fields[1]; got != "/v/.undo/S/1-1" {
		t.Errorf("backup before the move = %q, want /v/.undo/S/1-1", got)
	}
	if got := out[2].Fields[1]; got != "/v/.undo/S/1-2" {
		t.Errorf("backup after the move = %q, should be untouched", got)
	}
}

func TestResolveStoreMovesChains(t *testing.T) {
	in := []Entry{
		{Op: OpUnlink, Fields: []string{"/v/a/b/c/x", "/v/a/b/c/.undo/S/1-1", "link"}},
		{Op: OpStoreMove, Fields: []string{"/v/a/b/c/.undo/S", "/v/a/b/.undo/S"}},
		{Op: OpStoreMove, Fields: []string{"/v/a/b/.undo/S", "/v/a/.undo/S"}},
	}
	out := ResolveStoreMoves(in)
	if got := out[0].Fields[1]; got != "/v/a/.undo/S/1-1" {
		t.Errorf("chained move = %q, want /v/a/.undo/S/1-1", got)
	}
}

func TestResolveStoreMovesMarksDiscarded(t *testing.T) {
	in := []Entry{
		{Op: OpUnlink, Fields: []string{"/v/p/a.txt", "/v/p/.undo/S/1-1", "link"}},
		{Op: OpStoreMove, Fields: []string{"/v/p/.undo/S", "-"}},
	}
	out := ResolveStoreMoves(in)
	if got := out[0].Fields[1]; got != "-" {
		t.Errorf("discarded backup = %q, want %q", got, "-")
	}
}

func TestResolveStoreMovesRespectsComponentBoundaries(t *testing.T) {
	in := []Entry{
		// /v/p2 must not be rewritten by a move of /v/p
		{Op: OpUnlink, Fields: []string{"/v/x", "/v/p2/.undo/S/1-1", "link"}},
		{Op: OpStoreMove, Fields: []string{"/v/p/.undo/S", "/v/.undo/S"}},
	}
	out := ResolveStoreMoves(in)
	if got := out[0].Fields[1]; got != "/v/p2/.undo/S/1-1" {
		t.Errorf("unrelated prefix was rewritten to %q", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh go test ./internal/journal/ -v`

Expected: compile failure — `OpStoreMove`, `Method`, and `ResolveStoreMoves`
are undefined.

- [ ] **Step 3: Implement in `internal/journal/journal.go`**

Add to the op constants:

```go
	OpLost      = "lost"     // path, why       -> warn only
	OpStoreMove = "storemv"  // old-prefix, new-prefix|- -> backups moved
```

Add after the `Entry` type:

```go
// backupField returns the index of e's backup path field, or -1 if the op
// carries no backup.
func (e Entry) backupField() int {
	switch e.Op {
	case OpUnlink, OpMod:
		return 1
	case OpRename:
		return 2
	}
	return -1
}

// Backup returns the backup path this entry names, or "" when it names none
// and "-" when the backup was discarded.
//
// Every consumer that needs a backup path goes through this: which field holds
// it differs per op, and duplicating that knowledge in the session and restore
// packages is how the three drift apart when a new op is added.
func (e Entry) Backup() string {
	i := e.backupField()
	if i < 0 || i >= len(e.Fields) {
		return ""
	}
	return e.Fields[i]
}

// Method reports how the shim saved this entry's backup: "link" for a
// hardlink, which allocates nothing, "copy" for a full byte copy, or "none"
// when no backup was taken.
//
// A record with no method field predates the field and is read as "copy" --
// the pessimistic answer, and exactly the accounting those journals were
// written under.
func (e Entry) Method() string {
	i := e.backupField()
	if i < 0 {
		return "none"
	}
	if i+1 < len(e.Fields) {
		if m := e.Fields[i+1]; m != "" {
			return m
		}
	}
	if i < len(e.Fields) && (e.Fields[i] == "" || e.Fields[i] == "-") {
		return "none"
	}
	return "copy"
}

// hasPrefixPath reports whether path is pfx or lies beneath it, comparing on
// '/' boundaries so /v/p does not match /v/p2.
func hasPrefixPath(path, pfx string) bool {
	if path == pfx {
		return true
	}
	return len(path) > len(pfx) && strings.HasPrefix(path, pfx) &&
		path[len(pfx)] == '/'
}

// ResolveStoreMoves rewrites backup paths through the storemv records the shim
// appends when it has to move a store out of a directory being removed.
//
// A storemv only affects backups recorded before it: anything saved afterwards
// already went to the new location. A destination of "-" means the store could
// not be moved and was discarded, so those backups become "-" and restore
// reports them as gone rather than chasing a path that no longer exists.
//
// The returned slice has the same length and order as the input. Journal
// indices are load-bearing -- restore.slot() and --only are keyed by position
// -- so storemv records stay in place rather than being filtered out.
func ResolveStoreMoves(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.Op != OpStoreMove || len(e.Fields) < 2 {
			continue
		}
		from, to := e.Fields[0], e.Fields[1]
		if from == "" {
			continue
		}
		for j := 0; j < i; j++ {
			b := out[j].backupField()
			if b < 0 || b >= len(out[j].Fields) {
				continue
			}
			p := out[j].Fields[b]
			if p == "" || p == "-" || !hasPrefixPath(p, from) {
				continue
			}
			fields := make([]string, len(out[j].Fields))
			copy(fields, out[j].Fields)
			if to == "-" {
				fields[b] = "-"
			} else {
				fields[b] = to + p[len(from):]
			}
			out[j].Fields = fields
		}
	}
	return out
}
```

Add `storemv` to `Describe`:

```go
	case OpStoreMove:
		return "store     " + f(0) + " -> " + f(1)
```

- [ ] **Step 4: Apply it when a session loads**

In `internal/session/session.go`, in `load` (line 90), replace
`s.Entries = entries` with:

```go
	// Backups whose store had to move out of a directory being removed are
	// recorded at their original path plus a storemv record. Resolving here
	// means restore, gc, and Remove all see corrected paths without knowing
	// the mechanism exists.
	s.Entries = journal.ResolveStoreMoves(entries)
```

- [ ] **Step 5: Make restore report a discarded backup honestly**

In `internal/restore/restore.go`, add a guard at the top of the `OpUnlink` and
`OpMod` cases, and a no-op arm for `storemv`.

In `case journal.OpUnlink:`, before the `if dir == Undo {`:

```go
			if field(1) == "" || field(1) == "-" {
				skip("the backup was discarded when its directory was removed")
				continue
			}
```

In `case journal.OpMod:`, before the `if !act() {`:

```go
			if field(1) == "" || field(1) == "-" {
				skip("the backup was discarded when its directory was removed")
				continue
			}
```

And alongside `case journal.OpLost:`:

```go
		case journal.OpStoreMove:
			// bookkeeping, not a change to replay
			continue
```

Without the last arm these records fall to `default:` and every replay reports
`unknown journal op` for each one.

- [ ] **Step 6: Run everything**

Run: `test/in-container.sh make test`

Expected: `go test ./...` passes with the five new journal cases, and every e2e
case passes.

- [ ] **Step 7: Commit**

```bash
git add internal/journal/journal.go internal/journal/journal_test.go \
        internal/session/session.go internal/restore/restore.go
git commit -m 'journal: read the save method, and follow a store that moved

The shim now appends a method field to every backup-bearing record and writes
a storemv record for each backup it has to copy off a volume whose store is
being destroyed. Method() reads the former, defaulting to "copy" for journals written
before the field existed -- the pessimistic answer, and exactly the
accounting those journals were written under.

ResolveStoreMoves applies the latter to every backup recorded before the move,
on / component boundaries so a move of /v/p does not rewrite /v/p2. Chained
moves compose in journal order, which is what rm -rf produces as it walks up.
A destination of "-" means the store could not be moved and was discarded;
those backups become "-" and restore says so instead of reporting a bare
ENOENT for a path that no longer exists.

Records are rewritten in place, never filtered: restore.slot() and --only are
keyed by journal position, so dropping a record would silently retarget every
selective replay after it.'
```

---

### Task 6: Prove placement, containment, and evacuation on two filesystems

**Files:**
- Create: `test/multifs-store.sh`

**Interfaces:**
- Consumes: `test/multifs.sh` from Phase 1, which exports `FS_A` and `FS_B`.
- Produces: `test/multifs-store.sh`.

- [ ] **Step 1: Write the assertion script**

Create `test/multifs-store.sh`:

```bash
#!/usr/bin/env bash
# Store placement on two real filesystems: the session directory on one, the
# files on the other, so a backup that is NOT filesystem-local is provably a
# cross-device copy rather than a hardlink.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-store.sh'
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
LIB=$ROOT/build/libundo.so
[[ -x $UNDO && -f $LIB ]] || fail "run make first"

export UNDO_DATA_DIR=$FS_B/undo-data
mkdir -p "$UNDO_DATA_DIR"

# Arms the shim exactly the way test/e2e.sh does, so the two suites agree on
# what "armed" means. Takes one string, run through bash -c.
run_armed() {
    local id sess
    id=$(date +%s%N | cut -c1-16)
    sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "$*"
    sleep 0.01 # keep session ids strictly ordered
}

latest() { ls -1d "$UNDO_DATA_DIR"/sessions/* | sort | tail -1; }

echo "== the store lands on the file's own filesystem"
mkdir -p "$FS_A/user/project"
echo "content" >"$FS_A/user/project/f.txt"
run_armed "rm $FS_A/user/project/f.txt"
sess=$(latest)
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | tail -1)
[[ $bak == "$FS_A"/* ]] || fail "backup landed at $bak, not on $FS_A"
[[ $bak == *"/.undo/"* ]] || fail "backup is not inside a .undo store: $bak"

echo "== a deletion is hardlinked, not copied"
method=$(awk -F'\t' '$1=="unlink"{print $4}' "$sess/journal" | tail -1)
[[ $method == link ]] || fail "save method = '$method', want link"

echo "== the store is not inside the tree that was operated on"
case $bak in
    "$FS_A"/user/project/*) fail "the store is inside the operated tree: $bak" ;;
esac

echo "== rm -rf of the store's own directory still succeeds"
mkdir -p "$FS_A/user/tree/sub"
echo "recoverable" >"$FS_A/user/tree/sub/x.txt"
run_armed "rm -rf $FS_A/user/tree"
[[ ! -e $FS_A/user/tree ]] || fail "rm -rf left the tree behind"

echo "== and the backup survived the evacuation"
sess=$(latest)
"$UNDO" apply "$(basename "$sess")" -y >/dev/null || fail "undo failed"
[[ $(cat "$FS_A/user/tree/sub/x.txt") == "recoverable" ]] ||
    fail "the backup did not survive"

echo "== purge reclaims the distributed store"
"$UNDO" purge -y >/dev/null
found=$(find "$FS_A" -name '.undo' -type d 2>/dev/null | head -1)
[[ -z $found ]] || fail "purge left a store behind at $found"

echo
echo "store placement ok"
```

```bash
chmod +x test/multifs-store.sh
```

- [ ] **Step 2: Run it**

Run:

```bash
test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-store.sh'
```

Expected: the Phase 1 harness assertions, then all six `==` lines, then
`store placement ok`.

- [ ] **Step 3: Assert the invariant under an unwritable store**

The degradation ladder promises that an unwritable store costs a backup, never
the command. Append to `test/multifs-store.sh`:

```bash
echo "== an unwritable store never fails the user's command"
mkdir -p "$FS_A/ro/sub"
echo data >"$FS_A/ro/sub/f.txt"
chmod 500 "$FS_A/ro"
if ! run_armed "rm $FS_A/ro/sub/f.txt"; then
    chmod 700 "$FS_A/ro"
    fail "rm failed because the store was unwritable"
fi
chmod 700 "$FS_A/ro"
[[ ! -e $FS_A/ro/sub/f.txt ]] || fail "the rm did not actually happen"
```

Run it again. Expected: the new assertion passes.

- [ ] **Step 4: Scan and commit**

```bash
tools/check-no-site-data.sh test/multifs-store.sh && echo "scan clean"
git ls-files -z | xargs -0 tools/check-no-site-data.sh; echo "tree exit=$?"
git add test/multifs-store.sh
git commit -m 'test: store placement, containment, and evacuation on two filesystems

Puts the session directory on one tmpfs and the files on the other, so a
backup that is not filesystem-local is provably a cross-device copy rather
than a hardlink -- which makes "the save method is link" a real assertion
instead of a tautology.

Covers the four properties the design rests on: the store lands on the
file'"'"'s own filesystem, a deletion is hardlinked, the store is never inside
the tree being operated on, and rm -rf of the store'"'"'s own directory still
succeeds with the backup surviving the evacuation. Plus the invariant: an
unwritable store costs a backup, never the command.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes, including new e2e cases 25, 26 and 27
- [ ] `test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-store.sh'` prints `store placement ok`
- [ ] `test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-restore.sh'` still prints `cross-device restore ok` (2a did not regress)
- [ ] The shim's glibc floor is still `GLIBC_2.34` or lower after every task
- [ ] A deletion on a filesystem other than the session store's is recorded with method `link`, not `copy`
- [ ] `rmdir` of a directory whose last entry is our store succeeds, and the backups are still restorable afterwards
- [ ] `rm -rf` of a tree containing the store succeeds, and the files it deleted are still restorable
- [ ] `rm -rf` of a tree containing the store reports no missing files and returns its normal exit status — the backups are copied out, not moved out
- [ ] A backup over `UNDO_MAX_BYTES` in a destroyed store yields `storemv <path> -`, and restore reports it rather than failing on a missing path
- [ ] An unwritable store leaves the user's command's exit status untouched
- [ ] `errno` after a genuinely failed `rmdir` is still `ENOTEMPTY`, not something the evacuation path left behind
- [ ] A journal with no method field still restores, and reads as method `copy`
- [ ] `git ls-files -z | xargs -0 tools/check-no-site-data.sh` exits 0
- [ ] `tools/check-ere.sh` passes

## Deliberately not in this plan

- GC accounting by method, `UNDO_MAX_AGE`, the store-root registry, and the orphan sweep (2c)
- `FICLONE` and the copy ladder, the visible `lost` report, per-volume `doctor` (phase 3)
- Closing the TOCTOU window between resolving a path and linking it. The
  resolver stats `P`, walks its parents, then links `P`; components can be
  renamed or mounted over in between. The existing code already races
  (`undo_shim.c:380`) and this widens it. Documented, not solved.
- Symlink and bind-mount aliasing. `realpath` on every call is a network round
  trip the shim cannot afford, so candidates carrying `..` are rejected rather
  than normalized, and aliasing stays a known gap.

## Notes for the implementer

- **`test/pathpred.c` `#include`s the shim's `.c` file.** That is how the static
  predicates get tested without exporting them. It is a test harness, not part
  of the build; `make` does not reference it.
- **The containment guard fires less often than it reads.** `resolve_store_root`
  strips the last component before it starts walking, so a candidate is always a
  strict ancestor of the path — the guard cannot fire for an ordinary file
  unlink. It exists for rename destinations, for paths carrying `..`, and as the
  check applied to the `$UNDO_SESSION/data` fallback. **The mechanism that
  actually handles `rm -rf` is the evacuation in Task 4**, not this guard. Do not
  conclude the guard is dead code and remove it, and do not conclude it is
  sufficient and skip Task 4.
- **Backups are copied out, never moved out.** On the `rm -rf` path the
  originals have to stay so `rm` can delete them. Moving them makes `rm` report
  files that vanished and can change its exit status, which is the invariant
  this whole task exists to protect.
- **The evacuation is once per store per thread** (`evac_seen`), or a recursive
  delete re-copies the entire store once for every file it removes. The
  `remove_after` path deliberately bypasses that check, because it runs at most
  once per directory and must be exhaustive.
- **Watch `errno` everywhere in Task 4.** The retry path calls at least four
  syscalls between the failing `rmdir` and the caller's return. Invariant 1 is
  about more than the return value.
- The method token for a `rename` with no backup is `"none"`, emitted so the
  field position is fixed. A reader that counts fields to locate the method will
  be wrong on exactly this record.
