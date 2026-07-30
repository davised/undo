# undo multi-filesystem, Phase 2c — GC accounting by save method, and the orphan sweep

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the collector charging a free hardlink against the byte budget,
which is what makes the hardlink mechanism useless for exactly the large files
it exists to protect — and give it a way to reclaim distributed stores whose
session is gone.

**Architecture:** Session size stops being "walk the session directory" and
becomes "the session directory, plus every backup the journal names whose save
method allocated something". Hardlinks are excluded from the byte budget and
bounded instead by session count and a new age limit. A persistent registry of
store roots, accumulated from journals, lets `undo gc` sweep `.undo/<id>/`
directories whose session no longer exists.

**Tech Stack:** Go 1.24, bash (e2e and the two-filesystem harness).

## Global Constraints

Copied verbatim from `docs/design/undo-multifs-design.md` and `AGENTS.md`. Every
task inherits these.

- **The shim must never cause the user's command to fail.** (No shim change in
  this plan, but the constraint stands.)
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`.
- **No site-specific data in this repository** — no hostnames, mount points,
  organization domains, internal addresses, usernames, or storage capacities.
- **The journal format is append-only and additive.** Records must never be
  filtered out of the parsed entry list.
- **Deletion outside the store is the highest-risk operation in this plan.** GC
  derives paths from journal contents. A path must be proven to lie inside a
  store directory before anything removes it.
- Every build and test runs through `test/in-container.sh`; `test/multifs.sh`
  also needs `--privileged`.

## Depends on

Plan 2b, specifically `journal.Method()` and the `<root>/.undo/<session-id>/`
backup layout. Nothing here works before that lands.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/session/session.go` | modify | `sessionSize` replacing `dirSize`; `storeRoots`; `Roots`/`saveRoots` registry; `SweepOrphans`; `GC` gains an age limit |
| `internal/session/session_test.go` | modify (append) | accounting by method, age eviction, sweep, and the guards on it |
| `cmd/undo/main.go` | modify | `UNDO_MAX_AGE`; `cmdGC` calls the sweep and reports what it reclaimed |
| `test/e2e.sh` | modify (append case 27) | a large hardlinked backup survives gc while a large copied one does not |
| `test/multifs-gc.sh` | create | the sweep reclaiming a store on the other filesystem |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `UNDO_KEEP` | 30 | maximum sessions retained (existing) |
| `UNDO_MAX_STORE` | 1 GiB | maximum **allocated** bytes; hardlinks do not count (existing knob, new meaning) |
| `UNDO_MAX_AGE` | `0` (off) | maximum session age; `0` disables |

`UNDO_MAX_AGE` defaults to off deliberately. Hardlinked backups leave the byte
budget entirely in this plan, so the only thing bounding their deferred
reclamation would be `UNDO_KEEP`. An age limit is the right tool for that, but
switching one on by default would silently delete undo history that users
currently keep, on an upgrade that advertises no such thing. Deployment sets it;
the public default preserves existing behavior.

---

### Task 1: Charge only the bytes a backup actually allocated

`dirSize(s.Dir)` sums the session directory. That is wrong twice over now: it
misses backups that live on other filesystems entirely, and it counts a
hardlinked backup at full logical size — so a hardlinked 50 GiB deletion counts
50 GiB against a 1 GiB budget and is evicted on the very next command. The
mechanism that is otherwise free becomes the one that gets pruned first.

A hardlink allocates nothing: it is a second name for blocks that already
exist. Its cost is deferred reclamation, not allocation, which is what the age
limit in Task 2 bounds instead.

**Files:**
- Modify: `internal/session/session.go:236-248` (`dirSize`) and `:252-287` (`GC`)
- Test: `internal/session/session_test.go` (append)

**Interfaces:**
- Consumes: `journal.Entry.Method()` and `journal.Entry.Backup()` from plan 2b.
- Produces: `func (s *Session) allocatedBytes() int64`, `func (s *Session) Started() time.Time`, and `GC(keep int, maxBytes int64, maxAge time.Duration)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/session/session_test.go`:

```go
// writeBackup puts a file of n bytes at path, creating parents.
func writeBackup(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHardlinkedBackupsAreNotCharged(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	linked, _ := Create("rm huge.bin")
	linked.MarkDone()
	lb := filepath.Join(vol, ".undo", linked.ID, "1-1")
	writeBackup(t, lb, 1<<20)
	writeJournal(t, linked, "unlink\t"+filepath.Join(vol, "huge.bin")+"\t"+lb+"\tlink\n")

	copied, _ := Create("truncate huge.bin")
	copied.MarkDone()
	cb := filepath.Join(vol, ".undo", copied.ID, "1-1")
	writeBackup(t, cb, 1<<20)
	writeJournal(t, copied, "mod\t"+filepath.Join(vol, "huge.bin")+"\t"+cb+"\tcopy\n")

	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var gotLink, gotCopy int64 = -1, -1
	for _, s := range all {
		switch s.ID {
		case linked.ID:
			gotLink = s.allocatedBytes()
		case copied.ID:
			gotCopy = s.allocatedBytes()
		}
	}
	if gotLink != 0 {
		t.Errorf("hardlinked session charged %d bytes, want 0", gotLink)
	}
	if gotCopy < 1<<20 {
		t.Errorf("copied session charged %d bytes, want at least %d", gotCopy, 1<<20)
	}
}

func TestGCKeepsBigHardlinkAndPrunesBigCopy(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	// oldest: a large copy, must go
	bigCopy, _ := Create("truncate a.bin")
	bigCopy.MarkDone()
	p := filepath.Join(vol, ".undo", bigCopy.ID, "1-1")
	writeBackup(t, p, 1<<20)
	writeJournal(t, bigCopy, "mod\t"+filepath.Join(vol, "a.bin")+"\t"+p+"\tcopy\n")

	// middle: an equally large hardlink, must stay
	bigLink, _ := Create("rm b.bin")
	bigLink.MarkDone()
	q := filepath.Join(vol, ".undo", bigLink.ID, "1-1")
	writeBackup(t, q, 1<<20)
	writeJournal(t, bigLink, "unlink\t"+filepath.Join(vol, "b.bin")+"\t"+q+"\tlink\n")

	// newest: always survives, and keeps the two above from being "newest"
	newest, _ := Create("rm c.bin")
	newest.MarkDone()
	writeJournal(t, newest, "create\t"+filepath.Join(vol, "c.bin")+"\n")

	if _, err := GC(10, 4096, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bigCopy.Dir); !os.IsNotExist(err) {
		t.Error("a large copied backup should have been pruned by the byte budget")
	}
	if _, err := os.Stat(bigLink.Dir); err != nil {
		t.Error("a large hardlinked backup should not be charged against the byte budget")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh go test ./internal/session/ -run 'TestHardlinked|TestGCKeepsBig' -v`

Expected: compile failure — `allocatedBytes` is undefined and `GC` takes two
arguments, not three.

- [ ] **Step 3: Implement**

In `internal/session/session.go`, keep `dirSize` (the session directory still
has to be measured) and add after it:

```go
// allocatedBytes is what this session actually costs in space: the session
// directory, plus every backup the journal names whose save method allocated
// something.
//
// A hardlink is a second name for blocks that already exist, so it allocates
// nothing and is excluded. Counting one at full logical size -- which is what
// walking the directory does -- means a hardlinked 50 GiB deletion is charged
// 50 GiB against a 1 GiB budget and evicted on the next command, making the
// free mechanism the first one pruned. Hardlinks are bounded by session count
// and UNDO_MAX_AGE instead, since their cost is deferred reclamation.
//
// Only non-hardlinked backups are stat'd, which is also what keeps this cheap:
// a command deleting 100k files produces 100k hardlink records and zero round
// trips here, while copies are capped at UNDO_MAX_BYTES each and far rarer.
func (s *Session) allocatedBytes() int64 {
	total := dirSize(s.Dir)
	prefix := s.Dir + string(os.PathSeparator)
	for _, e := range s.Entries {
		if m := e.Method(); m == "link" || m == "none" {
			continue
		}
		b := e.Backup()
		if b == "" || b == "-" || strings.HasPrefix(b, prefix) {
			continue // no backup, discarded, or already counted by dirSize
		}
		if fi, err := os.Lstat(b); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
	}
	return total
}
```

Rewrite `backupPaths` (added in plan 2a) to use `journal.Entry.Backup()` from
plan 2b, so the two cannot drift apart. Which field holds the backup differs per
op, and that knowledge belongs in one place:

```go
func (s *Session) backupPaths() []string {
	var out []string
	for _, e := range s.Entries {
		p := e.Backup()
		if p == "" || p == "-" {
			continue
		}
		out = append(out, p)
	}
	return out
}
```

Then change `GC` to take a max age and use `allocatedBytes`:

```go
// GC removes empty sessions and prunes the oldest until at most keep sessions
// remain within maxBytes of allocated space. Sessions older than maxAge go
// regardless; maxAge of 0 disables that. Live sessions are never touched.
func GC(keep int, maxBytes int64, maxAge time.Duration) (int, error) {
	TightenPerms()
	all, err := List()
	if err != nil {
		return 0, err
	}
	removed, kept := 0, 0
	var total int64
	now := time.Now()
	for _, s := range all { // newest first
		if s.Live() {
			continue
		}
		if len(s.Entries) == 0 {
			if s.Remove() == nil {
				removed++
			}
			continue
		}
		kept++
		total += s.allocatedBytes()

		// The newest session is the one `undo` with no arguments targets, so
		// it always survives. Dropping it because a single big delete blew the
		// size budget would silently remove exactly the undo the user is about
		// to reach for.
		if kept == 1 {
			continue
		}
		tooOld := maxAge > 0 && now.Sub(s.Started()) > maxAge
		if kept > keep || total > maxBytes || tooOld {
			if s.Remove() == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// Started is when the session's command ran, decoded from its id -- which is
// unix seconds followed by six digits of microseconds.
func (s *Session) Started() time.Time {
	if len(s.ID) < 7 {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(s.ID[:len(s.ID)-6], 10, 64)
	if err != nil {
		return time.Time{}
	}
	usec, err := strconv.ParseInt(s.ID[len(s.ID)-6:], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, usec*1000)
}
```

Update the one caller in `cmd/undo/main.go:52-56`:

```go
func cmdGC(auto bool) {
	keep := int(envInt("UNDO_KEEP", 30))
	maxBytes := envInt("UNDO_MAX_STORE", 1<<30)
	maxAge := time.Duration(envInt("UNDO_MAX_AGE", 0)) * time.Second
	removed, err := session.GC(keep, maxBytes, maxAge)
	...
```

Add `"time"` to `main.go`'s imports if it is not already there.

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ ./cmd/... -v`

Expected: PASS, including the two new cases.

- [ ] **Step 5: Commit**

```bash
git add internal/session/session.go internal/session/session_test.go cmd/undo/main.go
git commit -m 'session: charge a backup only for the bytes it allocated

GC summed the session directory, which is wrong twice over once backups live
on the filesystem of the file they protect: it misses them entirely, and it
counts a hardlinked backup at full logical size. A hardlinked 50 GiB deletion
was therefore charged 50 GiB against a 1 GiB budget and evicted on the next
command -- making the free mechanism the first one pruned, precisely for the
large files it exists to protect.

A hardlink is a second name for blocks that already exist. It allocates
nothing, so it is excluded from the byte budget and bounded by session count
and the new UNDO_MAX_AGE instead, since its real cost is deferred
reclamation rather than allocation.

Only non-hardlinked backups are stat'd, which also keeps this cheap on
network storage: a command deleting 100k files costs zero round trips here.'
```

---

### Task 2: Reclaim stores whose session is gone

Distributed backups are deleted by reading the session's journal (plan 2a). A
lost, truncated, or hand-deleted journal therefore orphans them beyond anything's
reach, and the space is unrecoverable without manual cleanup on every volume the
user ever touched.

The roots are recoverable without any new shim machinery: every backup path has
the form `<root>/.undo/<session-id>/<name>`, so truncating at the `.undo`
component yields the root. Because a root must stay sweepable after the last
session that used it has been pruned, `gc` accumulates them into a persistent
registry.

**Files:**
- Modify: `internal/session/session.go`
- Test: `internal/session/session_test.go` (append)

**Interfaces:**
- Consumes: `journal.Entry.Backup()` from plan 2b.
- Produces: `storeRoots(entries []journal.Entry) []string`, `Roots() []string`, `rememberRoots(roots []string) error`, `SweepOrphans() (int, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/session/session_test.go`:

```go
func TestSweepRemovesOrphanedStores(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	live, _ := Create("rm keep.txt")
	live.MarkDone()
	lb := filepath.Join(vol, ".undo", live.ID, "1-1")
	writeBackup(t, lb, 16)
	writeJournal(t, live, "unlink\t"+filepath.Join(vol, "keep.txt")+"\t"+lb+"\tlink\n")

	// a store whose session directory no longer exists: exactly what a lost
	// journal leaves behind
	orphan := filepath.Join(vol, ".undo", "1700000000000001")
	writeBackup(t, filepath.Join(orphan, "1-1"), 16)

	// teach the registry about this root by running a gc first
	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	n, err := SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d stores, want 1", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphaned store survived the sweep")
	}
	if _, err := os.Stat(lb); err != nil {
		t.Error("the sweep removed a store belonging to a live session")
	}
}

func TestSweepIgnoresNonSessionDirectories(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	s, _ := Create("rm x")
	s.MarkDone()
	b := filepath.Join(vol, ".undo", s.ID, "1-1")
	writeBackup(t, b, 16)
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "x")+"\t"+b+"\tlink\n")

	// something else living under .undo that is not ours to delete
	notOurs := filepath.Join(vol, ".undo", "notes")
	writeBackup(t, filepath.Join(notOurs, "readme"), 4)

	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(notOurs, "readme")); err != nil {
		t.Error("the sweep deleted a directory whose name is not a session id")
	}
}

func TestRootsRegistrySurvivesSessionPruning(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	s, _ := Create("rm y")
	s.MarkDone()
	b := filepath.Join(vol, ".undo", s.ID, "1-1")
	writeBackup(t, b, 16)
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "y")+"\t"+b+"\tlink\n")

	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	// drop every session; the root must still be known
	if err := os.RemoveAll(Root()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range Roots() {
		if r == vol {
			found = true
		}
	}
	if !found {
		t.Fatalf("root %q was forgotten once its sessions were gone; Roots() = %v", vol, Roots())
	}
}

func TestSweepKeepsUnreachableRoots(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	gone := filepath.Join(t.TempDir(), "unmounted")
	if err := rememberRoots([]string{gone}); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepOrphans(); err != nil {
		t.Fatal(err)
	}
	// An unmounted volume must not be mistaken for a reclaimed one: forgetting
	// it strands whatever it holds forever.
	found := false
	for _, r := range Roots() {
		if r == gone {
			found = true
		}
	}
	if !found {
		t.Error("an unreachable root was dropped from the registry")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh go test ./internal/session/ -run 'TestSweep|TestRoots' -v`

Expected: compile failure — `SweepOrphans`, `Roots`, and `rememberRoots` are
undefined.

- [ ] **Step 3: Implement**

Add to `internal/session/session.go`:

```go
// storeDirName is the per-filesystem store directory the shim creates.
const storeDirName = ".undo"

// rootsFile is the registry of store roots, kept beside the sessions
// directory so it outlives any individual session.
func rootsFile() string {
	return filepath.Join(filepath.Dir(Root()), "roots")
}

// sessionID reports whether name has the shape session.Create produces:
// unix seconds followed by six digits of microseconds, all decimal.
//
// The sweep deletes directories, so this is a guard, not a formatting detail.
func sessionID(name string) bool {
	if len(name) < 7 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// storeRoots extracts the filesystem-local store roots a journal implies.
// Backups are written to <root>/.undo/<session-id>/<name>, so the root is
// whatever precedes the .undo component.
func storeRoots(entries []journal.Entry) []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range entries {
		p := e.Backup()
		if p == "" || p == "-" || !filepath.IsAbs(p) {
			continue
		}
		// <root>/.undo/<id>/<name> -> <root>
		idDir := filepath.Dir(p)
		undoDir := filepath.Dir(idDir)
		if filepath.Base(undoDir) != storeDirName {
			continue
		}
		root := filepath.Dir(undoDir)
		if root == "" || root == "/" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out
}

// Roots returns the store roots this installation knows about.
func Roots() []string {
	b, err := os.ReadFile(rootsFile())
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !filepath.IsAbs(line) || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// rememberRoots adds roots to the registry, keeping what is already there.
//
// The registry exists because a root has to stay sweepable after the last
// session that used it is pruned -- which is exactly when its orphans become
// unreachable by any other means.
func rememberRoots(roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	all := Roots()
	seen := make(map[string]bool, len(all))
	for _, r := range all {
		seen[r] = true
	}
	changed := false
	for _, r := range roots {
		if !filepath.IsAbs(r) || seen[r] {
			continue
		}
		seen[r] = true
		all = append(all, r)
		changed = true
	}
	if !changed {
		return nil
	}
	sort.Strings(all)
	if err := os.MkdirAll(filepath.Dir(rootsFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(rootsFile(), []byte(strings.Join(all, "\n")+"\n"), 0o600)
}

// SweepOrphans removes store directories whose session no longer exists, and
// forgets roots that are reachable and hold nothing.
//
// Deletion here is driven by directory names on disk rather than by a journal,
// so it is fenced three ways: the directory must sit directly under a .undo
// directory beneath a registered root, its name must have the shape of a
// session id, and no session by that name may exist.
//
// A root that cannot be read is kept, not forgotten. An unmounted volume is
// indistinguishable from an empty one from here, and dropping it would strand
// whatever it holds permanently. On node-local filesystems this reclaims only
// on the node that created the store, since no other node can see the path.
func SweepOrphans() (int, error) {
	roots := Roots()
	if len(roots) == 0 {
		return 0, nil
	}
	removed := 0
	var keep []string
	for _, root := range roots {
		undoDir := filepath.Join(root, storeDirName)
		ents, err := os.ReadDir(undoDir)
		if err != nil {
			if os.IsNotExist(err) {
				// Reachable and empty: only then is it safe to forget.
				if _, serr := os.Stat(root); serr == nil {
					continue
				}
			}
			keep = append(keep, root) // unreadable, unmounted, or in use
			continue
		}
		live := 0
		for _, e := range ents {
			if !e.IsDir() || !sessionID(e.Name()) {
				live++ // not ours; its presence keeps the root registered
				continue
			}
			if _, err := os.Stat(filepath.Join(Root(), e.Name())); err == nil {
				live++
				continue
			}
			if os.RemoveAll(filepath.Join(undoDir, e.Name())) == nil {
				removed++
			}
		}
		if live > 0 {
			keep = append(keep, root)
			continue
		}
		os.Remove(undoDir) // only succeeds when empty
	}
	if len(keep) != len(roots) {
		sort.Strings(keep)
		body := ""
		if len(keep) > 0 {
			body = strings.Join(keep, "\n") + "\n"
		}
		if err := os.WriteFile(rootsFile(), []byte(body), 0o600); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
```

In `GC`, record each surviving session's roots before the prune loop can drop
them. Immediately after `all, err := List()` and its error check, add:

```go
	// Learn the roots before anything is pruned: a session removed below takes
	// its journal, and with it the only record of where its backups lived.
	var roots []string
	for _, s := range all {
		roots = append(roots, storeRoots(s.Entries)...)
	}
	rememberRoots(roots)
```

`sort` and `strings` are already imported.

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ -v`

Expected: PASS, all four new cases plus the existing ones.

- [ ] **Step 5: Call the sweep from `undo gc`**

In `cmd/undo/main.go`, in `cmdGC`, after the `session.GC` call and its error
handling:

```go
	swept, serr := session.SweepOrphans()
	if serr == nil && swept > 0 {
		fmt.Printf("reclaimed %d orphaned backup store(s)\n", swept)
	}
```

Keep it non-fatal: a volume that cannot be read is a normal condition on a
machine with network mounts, and it must not turn `undo gc` into an error.

- [ ] **Step 6: Run everything**

Run: `test/in-container.sh make test`

Expected: `go test ./...` passes and `all cases passed`.

- [ ] **Step 7: Commit**

```bash
git add internal/session/session.go internal/session/session_test.go cmd/undo/main.go
git commit -m 'session: reclaim backup stores whose session is gone

Distributed backups are deleted by reading the session journal, so a lost or
truncated journal orphans them beyond anything'"'"'s reach -- unrecoverable
without manual cleanup on every volume the user ever touched.

The roots need no new shim machinery: a backup path is
<root>/.undo/<session-id>/<name>, so truncating at .undo yields the root.
gc accumulates them into a registry beside the session store, because a root
has to stay sweepable after the last session that used it is pruned, which is
exactly when its orphans become unreachable by any other means.

The sweep deletes directories found on disk rather than paths named by a
journal, so it is fenced three ways: directly under a .undo beneath a
registered root, a name shaped like a session id, and no live session by that
name. A root that cannot be read is kept rather than forgotten -- an
unmounted volume is indistinguishable from an empty one from here, and
dropping it would strand its contents permanently.'
```

---

### Task 3: Prove the accounting end to end

**Files:**
- Modify: `test/e2e.sh` (append case 27)
- Create: `test/multifs-gc.sh`

**Interfaces:**
- Consumes: everything above.
- Produces: `test/multifs-gc.sh`.

- [ ] **Step 1: Append the e2e case**

Append to `test/e2e.sh`, after case 26:

```bash
echo "== case 27: gc keeps a big hardlinked backup and prunes a big copied one"
mkdir -p "$PLAY/acct"
dd if=/dev/zero of="$PLAY/acct/deleted.bin" bs=1M count=4 status=none
dd if=/dev/zero of="$PLAY/acct/rewritten.bin" bs=1M count=4 status=none
run_armed "rm $PLAY/acct/deleted.bin"
del_sess=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
run_armed "echo small > $PLAY/acct/rewritten.bin"
mod_sess=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
run_armed "touch $PLAY/acct/newest.txt"   # so neither of the above is newest

# a budget far below either backup's logical size: only the copy should count
UNDO_MAX_STORE=1048576 "$UNDO" gc >/dev/null
[[ -d $UNDO_DATA_DIR/sessions/$del_sess ]] ||
    fail "gc pruned a hardlinked backup, which allocates nothing"
[[ ! -d $UNDO_DATA_DIR/sessions/$mod_sess ]] ||
    fail "gc kept a copied backup that blew the byte budget"
```

- [ ] **Step 2: Run it**

Run: `test/in-container.sh test/e2e.sh`

Expected: all cases pass including 27.

If case 27 fails because the `rm` produced a copy rather than a hardlink, the
store did not land on `$PLAY`'s filesystem — check the journal's method field
before changing the test. `awk -F'\t' '$1=="unlink"{print $4}'` on the session's
journal says which happened.

- [ ] **Step 3: Write the sweep test on two filesystems**

Create `test/multifs-gc.sh`:

```bash
#!/usr/bin/env bash
# The orphan sweep reclaiming a store on a filesystem other than the one
# holding the session directory.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-gc.sh'
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
LIB=$ROOT/build/libundo.so
[[ -x $UNDO && -f $LIB ]] || fail "run make first"

export UNDO_DATA_DIR=$FS_B/undo-data
mkdir -p "$UNDO_DATA_DIR"

run_armed() {
    local id sess
    id=$(date +%s%N | cut -c1-16)
    sess=$UNDO_DATA_DIR/sessions/$id
    mkdir -p "$sess/data"
    echo "$*" >"$sess/cmd"
    env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" bash -c "$*"
    sleep 0.01
}

echo "== a store on the other filesystem is registered"
mkdir -p "$FS_A/user/work"
echo content >"$FS_A/user/work/f.txt"
run_armed "rm $FS_A/user/work/f.txt"
"$UNDO" gc >/dev/null
grep -q "^$FS_A" "$UNDO_DATA_DIR/roots" ||
    fail "the store root on $FS_A was not registered: $(cat "$UNDO_DATA_DIR/roots" 2>&1)"

echo "== an orphaned store is reclaimed"
undodir=$(find "$FS_A" -type d -name .undo | head -1)
[[ -n $undodir ]] || fail "no .undo directory was created on $FS_A"
orphan=$undodir/1700000000000001
mkdir -p "$orphan"
echo stranded >"$orphan/1-1"
"$UNDO" gc >/dev/null
[[ ! -e $orphan ]] || fail "the orphaned store was not reclaimed"

echo "== a live session's store is left alone"
live=$(ls -1 "$UNDO_DATA_DIR/sessions" | sort | tail -1)
[[ -d $undodir/$live ]] || fail "the sweep removed a live session's store"

echo
echo "orphan sweep ok"
```

```bash
chmod +x test/multifs-gc.sh
```

- [ ] **Step 4: Run it**

Run:

```bash
test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-gc.sh'
```

Expected: the Phase 1 harness assertions, the three `==` lines, then
`orphan sweep ok`.

- [ ] **Step 5: Scan and commit**

```bash
tools/check-no-site-data.sh test/multifs-gc.sh test/e2e.sh && echo "scan clean"
git ls-files -z | xargs -0 tools/check-no-site-data.sh; echo "tree exit=$?"
git add test/e2e.sh test/multifs-gc.sh
git commit -m 'test: accounting by save method, and the orphan sweep

Case 27 is the executable form of the finding the design rests on: with a
budget far below either backup, the hardlinked one survives and the copied
one is pruned. Before this, both were charged full logical size and the
hardlink -- the free mechanism -- was evicted first.

multifs-gc.sh proves the sweep across a real filesystem boundary: the store
on one tmpfs, the session directory on the other, an orphan reclaimed, and a
live session'"'"'s store left alone.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes, including new e2e case 27
- [ ] `test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-gc.sh'` prints `orphan sweep ok`
- [ ] 2a and 2b harnesses still pass: `multifs-restore.sh` and `multifs-store.sh`
- [ ] A hardlinked backup contributes 0 bytes to `allocatedBytes()`, a copied one contributes its size
- [ ] With `UNDO_MAX_STORE` below either backup, gc prunes the copy and keeps the hardlink
- [ ] `UNDO_MAX_AGE=0` (the default) evicts nothing by age; a non-zero value evicts sessions older than it
- [ ] The sweep removes an orphaned `<root>/.undo/<id>/` and leaves a live session's store, a non-session-named directory, and another user's content alone
- [ ] A store root stays in the registry after every session that used it is pruned
- [ ] An unreachable root is kept in the registry, not forgotten
- [ ] `git ls-files -z | xargs -0 tools/check-no-site-data.sh` exits 0

## Deliberately not in this plan

- `FICLONE` and the copy ladder. When it lands, `reflink` joins `link` and
  `copy` as a method token; it is counted at **full size** like a copy, because
  the original is about to be overwritten and the shared extents diverge.
  `allocatedBytes` needs no change for that — only `link` is excluded.
- The visible `lost` report, per-volume `doctor` (phase 3).
- Recording backup sizes in the journal to avoid stat'ing copies at GC time.
  Deletions are the common case and are hardlinks, which are never stat'd, so
  there is nothing to buy yet.

## Notes for the implementer

- **`GC`'s signature changes.** It is called from exactly one place
  (`cmd/undo/main.go:52-56`) plus the tests. Update all of them.
- **The registry has to be written before the prune loop, not after.** A pruned
  session takes its journal with it, and that journal is the only record of
  where its backups lived. Recording afterwards loses exactly the roots that
  most need sweeping.
- **`sessionID` is a safety fence, not formatting.** The sweep calls
  `os.RemoveAll` on directories it finds on disk. Loosening this check to
  something like "not a dotfile" turns a shared `.undo` directory into a hazard.
- **Keeping unreachable roots is deliberate and will look like a leak.** The
  registry grows on a machine with many network mounts and never shrinks while
  they are down. That is the correct trade: forgetting a root strands its
  contents permanently, while an extra line in a text file costs nothing.
- `Started()` decodes the session id rather than stat'ing the directory, because
  the directory's mtime changes when markers are written and would make a
  session look younger than its command.
