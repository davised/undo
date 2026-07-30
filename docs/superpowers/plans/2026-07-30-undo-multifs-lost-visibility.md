# undo multi-filesystem — making unprotected files visible

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tell the user, before they rely on a restore, which files a session cannot bring back — so a recorded loss stops being a loss they only discover while trying to recover.

**Architecture:** One function on `session.Session` counts the entries that cannot be restored, and three existing CLI surfaces report it: `undo run` after a capture, `undo list` per session, and the pre-revert preview. One shim change comes first, because a rename whose target backup failed is currently journaled identically to a rename that overwrote nothing — a loss the CLI cannot report until the journal distinguishes it. No new journal field or op: it uses a third value of the existing method field.

**Tech Stack:** Go 1.24, bash (e2e suite).

## Why this is being done now

This is design item 5 ("Make unprotected files visible"), listed under phase 3, pulled forward. Two things made it urgent rather than cosmetic:

- Phase 2b's store evacuation records a discarded backup as `storemv <path> -` when the file exceeds `UNDO_MAX_BYTES`. That is the right trade — the alternative breaks the user's `rm -rf` — but until it is surfaced, "recorded" and "silent" look identical to the user.
- A review finding on 2b was rejected specifically *because* the loss is recorded rather than silent. That argument only holds if something eventually shows it.

## Global Constraints

Copied from `docs/design/undo-multifs-design.md` and `AGENTS.md`. Every task inherits these.

- **The shim must never cause the user's command to fail.** Task 1 touches the shim but adds no failure path — it changes which string is journaled. The *reporting* stays in the CLI deliberately: writing to stderr from inside every process a user runs risks corrupting output that scripts parse.
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`.
- **No site-specific data in this repository.**
- **The journal format is append-only and additive.** This plan adds no fields and no ops — only a third accepted value, `lost`, of the method field 2b introduced.
- Every build and test runs through `test/in-container.sh`.

## What counts as unprotected

Three journal states, one rule:

| Journal state | Meaning |
|---|---|
| `lost` record | the shim could not save a backup at all — over `UNDO_MAX_BYTES`, or unlinkable |
| backup field `-`, method `link` or `copy` | a backup existed and was discarded when its store was destroyed |
| backup field `-`, method `lost` | a target was overwritten and the shim could not save it (Task 1) |

All of these reduce to one rule: **a `-` backup with any method other than
`none` is unprotected.** The method field is what carries the distinction, and
it has to be consulted rather than just testing for `-`, because
`rename old new - none` is a rename that overwrote nothing, needed no backup,
and restores perfectly. It is the most common entry in a real journal; counting
it would make the warning fire constantly.

`unlink` and `mod` records are only ever written with a real backup, so `-`
there always means discarded.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `shim/undo_shim.c` | modify | record a rename whose target backup failed as method `lost`, not `none` |
| `internal/session/session.go` | modify | `Unprotected()` — count entries that cannot be restored |
| `internal/session/session_test.go` | modify (append) | the counting rules, including the rename distinction |
| `cmd/undo/main.go` | modify | `cmdList` marks affected sessions; `previewSession` warns before the prompt |
| `cmd/undo/run.go` | modify | `undo run` reports the count after a capture |
| `test/e2e.sh` | modify (append cases 29, 30) | a failed rename-target save is recorded; an over-cap overwrite is reported by list and by the preview |

## Interfaces produced

```go
// internal/session
func (s *Session) Unprotected() int   // entries this session cannot restore
```

---

### Task 1: Record a rename whose target could not be saved

`handle_rename_pre` sets `kind = 1`, then upgrades to `kind = 2` only when
`save_file` succeeds on an existing regular destination. When the save *fails*
— the target is over `UNDO_MAX_BYTES`, or the store is unwritable — `kind`
stays 1 and `handle_rename_post` writes `rename old new - none`: byte for byte
what it writes when there was no destination to overwrite at all.

So a rename that clobbered a file whose backup could not be saved is recorded
as a rename that clobbered nothing. The file is unrecoverable and **nothing in
the journal says so**. `unlink` and the open family both have a failure path
that emits a `lost` record; rename never got one.

This is inherited from upstream, not introduced by the multi-filesystem work,
but it has to be fixed first: the CLI cannot report a loss the journal does not
distinguish.

The fix is a third method token. Both consumers already handle it —
`Unprotected()` counts any `-` backup whose method is not `none`, and
`restore.Run`'s `OpRename` arm already skips and reports exactly that state.

**Files:**
- Modify: `shim/undo_shim.c` — `handle_rename_pre`, `handle_rename_post`
- Test: `test/e2e.sh` (append case 29)

**Interfaces:**
- Consumes: `save_file`'s `method` out-parameter from plan 2b.
- Produces: the journal state `rename <old> <new> - lost`.

- [ ] **Step 1: Write the failing e2e case**

Append to `test/e2e.sh`, before the closing `echo "all cases passed"`:

```bash
echo "== case 29: a rename whose target could not be saved is recorded as lost"
mkdir -p "$PLAY/ren"
echo "victim content" >"$PLAY/ren/target.txt"
echo "source" >"$PLAY/ren/src.txt"
# A cap below the target size makes save_file fail, which is what happens to a
# large in-place overwrite on a filesystem with no reflink support.
id=$(date +%s%N | cut -c1-16); sess="$UNDO_DATA_DIR/sessions/$id"
mkdir -p "$sess/data"; echo "mv over target" >"$sess/cmd"
env UNDO_SESSION="$sess" UNDO_MAX_BYTES=4 LD_PRELOAD="$LIB" \
    bash -c "mv $PLAY/ren/src.txt $PLAY/ren/target.txt"
sleep 0.01
grep -q '^rename' "$sess/journal" ||
    fail "no rename record, journal: $(cat "$sess/journal")"
awk -F'\t' '$1=="rename"{print $5}' "$sess/journal" | grep -qx lost ||
    fail "rename method = '$(awk -F'\t' '$1=="rename"{print $5}' "$sess/journal")', want lost"
```

- [ ] **Step 2: Run it and watch it fail**

Run: `test/in-container.sh bash -c 'make >/dev/null 2>&1 && test/e2e.sh'`

Expected: `FAIL: rename method = 'none', want lost`. That literal `none` is the
defect: it is what the journal also records when nothing was overwritten.

- [ ] **Step 3: Implement**

In `shim/undo_shim.c`, in `handle_rename_pre`, replace the final block:

```c
    *kind = 1;
    struct stat st;
    if (lstat(absnew, &st) == 0 && S_ISREG(st.st_mode)) {
        if (save_file(absnew, 0, bak, method) == 0)
            *kind = 2;
        else
            /* A target existed and could not be saved. Without this the record
             * is identical to a rename that overwrote nothing, and the
             * clobbered file is unrecoverable with nothing saying so. */
            *method = "lost";
    }
```

and in `handle_rename_post`, make the `kind == 1` arm carry the method instead
of a hardcoded `"none"`:

```c
        if (kind == 1)
            jwrite("rename", absold, absnew, "-", method, NULL);
```

`save_file` sets `*method = "none"` on entry and only changes it on success, so
a rename with no target to save still records `none`.

- [ ] **Step 4: Run the suite**

Run: `test/in-container.sh make test`

Expected: all cases pass, including 29. Case 3 (`mv over an existing file`)
covers the ordinary path and must still pass — it has no cap set, so the save
succeeds and the record keeps a real backup and method.

- [ ] **Step 5: Confirm the floor did not move**

Run:

```bash
test/in-container.sh bash -c '
  gcc -shared -fPIC -O2 -Wall -Wextra -o /tmp/libundo.so shim/undo_shim.c -ldl
  objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
```

Expected: `GLIBC_2.34`. No new libc calls are introduced here, so anything else
means something unrelated changed.

- [ ] **Step 6: Commit**

```bash
git add shim/undo_shim.c test/e2e.sh
git commit -m 'shim: record a rename whose target backup failed

handle_rename_pre upgrades kind to 2 only when save_file succeeds on an
existing regular destination. When the save failed -- target over
UNDO_MAX_BYTES, or an unwritable store -- kind stayed 1 and the record written
was "rename old new - none", byte for byte what a rename that overwrote
nothing produces.

So a rename that clobbered an unsaveable file was recorded as a rename that
clobbered nothing: the file is unrecoverable and the journal did not say so.
unlink and the open family both emit a lost record on that path; rename never
had one. Inherited from upstream rather than new here.

Recorded as method "lost" instead, which both consumers already understand:
the unprotected count is any "-" backup whose method is not none, and restore
already skips and reports exactly that state rather than half-reverting.'
```

---

### Task 2: Count what cannot be restored

**Files:**
- Modify: `internal/session/session.go`
- Test: `internal/session/session_test.go` (append)

**Interfaces:**
- Consumes: `journal.Entry.Backup()` and `journal.Entry.Method()` from plan 2b, and the `lost` method token from Task 1.
- Produces: `func (s *Session) Unprotected() int`.

- [ ] **Step 1: Write the failing test**

Append to `internal/session/session_test.go`:

```go
func TestUnprotectedCounting(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{"a clean deletion", "unlink\t/f\t/v/.undo/S/1-1\tlink\n", 0},
		{"the shim saved nothing", "lost\t/f\twrite\n", 1},
		{"a discarded backup", "unlink\t/f\t-\tlink\n", 1},
		{"a discarded mod backup", "mod\t/f\t-\tcopy\n", 1},
		// A rename that overwrote nothing legitimately has no backup. It is
		// fully restorable and must not be counted, which is why the method
		// field has to be consulted rather than just the "-".
		{"a rename that overwrote nothing", "rename\t/a\t/b\t-\tnone\n", 0},
		{"a rename whose backup was discarded", "rename\t/a\t/b\t-\tlink\n", 1},
		{"a journal predating the method field", "unlink\t/f\t/v/.undo/S/1-1\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("UNDO_DATA_DIR", t.TempDir())
			s, err := Create("cmd")
			if err != nil {
				t.Fatal(err)
			}
			writeJournal(t, s, c.line)
			reloaded, err := Get(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.Unprotected(); got != c.want {
				t.Errorf("Unprotected() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestUnprotectedCountsAcrossAWholeSession(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm -rf big")
	if err != nil {
		t.Fatal(err)
	}
	writeJournal(t, s,
		"unlink\t/v/a\t/v/.undo/S/1-1\tlink\n"+
			"lost\t/v/huge.bin\twrite\n"+
			"unlink\t/v/b\t-\tlink\n"+
			"rename\t/v/c\t/v/d\t-\tnone\n")
	reloaded, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Unprotected(); got != 2 {
		t.Errorf("Unprotected() = %d, want 2", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `test/in-container.sh go test ./internal/session/ -run TestUnprotected -v`

Expected: compile failure — `Unprotected` is undefined.

- [ ] **Step 3: Implement**

Add to `internal/session/session.go`, after `backupPaths`:

```go
// Unprotected counts the entries this session cannot restore.
//
// Two things put an entry here, both already recorded by the shim: a `lost`
// record, meaning no backup could be taken at all (over UNDO_MAX_BYTES, or
// unlinkable), and a backup that existed and was discarded when its store was
// destroyed, which reads as a "-" backup field.
//
// The method field has to be consulted for the second: `rename old new - none`
// is a rename that overwrote nothing, needed no backup, and restores perfectly.
// Counting it would cry wolf on the most ordinary mv in the journal.
func (s *Session) Unprotected() int {
	n := 0
	for _, e := range s.Entries {
		if e.Op == journal.OpLost {
			n++
			continue
		}
		if e.Backup() == "-" && e.Method() != "none" {
			n++
		}
	}
	return n
}
```

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ -v`

Expected: PASS, including both new cases and every pre-existing one.

- [ ] **Step 5: Commit**

```bash
git add internal/session/session.go internal/session/session_test.go
git commit -m 'session: count the entries a session cannot restore

Two things make an entry unrecoverable, and the shim already records both: a
lost record, meaning no backup could be taken at all, and a backup that
existed and was discarded when its store was destroyed.

The method field distinguishes the second from an ordinary mv. A rename that
overwrote nothing carries no backup and restores perfectly; counting it would
cry wolf on the most common entry in any journal.'
```

---

### Task 3: Report it on the three surfaces

Three places, chosen because they are where a user forms the belief that they are protected: right after a command is captured, when listing what can be undone, and immediately before confirming a revert.

**Files:**
- Modify: `cmd/undo/main.go` — `cmdList`, `previewSession`
- Modify: `cmd/undo/run.go` — the capture message
- Test: `test/e2e.sh` (append case 29)

**Interfaces:**
- Consumes: `(*Session).Unprotected()` from Task 2.
- Produces: nothing new.

- [ ] **Step 1: Write the failing e2e case**

Append to `test/e2e.sh`, before the closing `echo "all cases passed"`:

```bash
echo "== case 30: files the shim could not save are reported, not just recorded"
mkdir -p "$PLAY/cap"
echo original >"$PLAY/cap/big.bin"
# UNDO_MAX_BYTES below the file size makes the shim record a lost entry
# instead of a backup, which is exactly what happens to a large in-place
# overwrite on a filesystem with no reflink support.
id=$(date +%s%N | cut -c1-16); sess="$UNDO_DATA_DIR/sessions/$id"
mkdir -p "$sess/data"; echo "overwrite big.bin" >"$sess/cmd"
env UNDO_SESSION="$sess" UNDO_MAX_BYTES=4 LD_PRELOAD="$LIB" \
    bash -c "echo replaced > $PLAY/cap/big.bin"
sleep 0.01
grep -q "^lost" "$sess/journal" || fail "expected a lost record, journal: $(cat "$sess/journal")"

out=$("$UNDO" list)
grep -q "unprotected" <<<"$out" || fail "undo list did not flag the session: $out"

out=$("$UNDO" -n 2>&1)
grep -q "cannot be restored" <<<"$out" || fail "the preview did not warn: $out"
```

- [ ] **Step 2: Run it and watch it fail**

Run: `test/in-container.sh bash -c 'make >/dev/null 2>&1 && test/e2e.sh'`

Expected: `FAIL: undo list did not flag the session`. If it fails earlier on the
`lost` record, the cap did not take effect — check that `UNDO_MAX_BYTES=4` is
in the armed environment and that the file is larger than 4 bytes.

- [ ] **Step 3: Mark affected sessions in `undo list`**

In `cmd/undo/main.go`, in `cmdList`, replace the `fmt.Printf` with:

```go
		note := ""
		if n := s.Unprotected(); n > 0 {
			note = fmt.Sprintf("  (%d unprotected)", n)
		}
		fmt.Printf("%s %s  %s  %3d changes  %s%s\n",
			mark, shortID(s.ID), when(s.ID), len(s.Entries), cmd, note)
```

- [ ] **Step 4: Warn in the pre-revert preview**

In `cmd/undo/main.go`, at the end of `previewSession`, after the "... and N more" block:

```go
	if n := s.Unprotected(); n > 0 {
		fmt.Printf("\n  warning: %d change(s) cannot be restored - "+
			"the backup was too large to save, or was discarded when its "+
			"store was removed\n", n)
	}
```

This lands above the `revert this? [y/N]` prompt, which is the point: the user
sees it while deciding, not after.

- [ ] **Step 5: Report it after a capture**

In `cmd/undo/run.go`, replace the capture message:

```go
	if fresh, err := session.Get(s.ID); err == nil && len(fresh.Entries) > 0 {
		msg := fmt.Sprintf("undo: captured %d change(s)", len(fresh.Entries))
		if n := fresh.Unprotected(); n > 0 {
			msg += fmt.Sprintf(", %d NOT protected", n)
		}
		fmt.Fprintln(os.Stderr, msg+", run 'undo' to revert")
	} else {
		s.Remove()
	}
```

Note the existing e2e case 8 greps for `captured 1 change`, which this still
contains as a prefix.

- [ ] **Step 6: Run everything**

Run: `test/in-container.sh make test`

Expected: `go test ./...` passes and every e2e case passes, including 29, 30,
and the pre-existing case 8.

- [ ] **Step 7: Commit**

```bash
git add cmd/undo/main.go cmd/undo/run.go test/e2e.sh
git commit -m 'cli: say which changes cannot be restored

The shim records a lost entry when it cannot save a backup, and 2b records a
discarded one when a store is destroyed with a backup too large to copy off.
Both were recorded and neither was shown, so "recorded" and "silent" looked
the same from the outside -- and the whole argument for the evacuation being
an acceptable trade is that the loss gets reported.

Reported on the three surfaces where a user forms the belief that they are
protected: after undo run captures a command, in undo list, and in the
pre-revert preview, which prints above the confirmation prompt so it is seen
while deciding rather than afterwards.

Deliberately not emitted from the shim: writing to stderr from inside every
process a user launches risks corrupting output that scripts parse. The shim
records; the CLI reports.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes, including new cases 29 and 30
- [ ] A rename whose target backup failed records method `lost`, not `none`
- [ ] A rename that overwrote nothing still records method `none`
- [ ] The shim's glibc floor is still `GLIBC_2.34`
- [ ] A session with a `lost` record is flagged in `undo list`
- [ ] The pre-revert preview warns above the confirmation prompt, not below it
- [ ] `undo run` reports the unprotected count alongside the captured count
- [ ] A `rename old new - none` entry is **not** counted as unprotected
- [ ] A `rename old new - link` entry **is** counted
- [ ] A `rename old new - lost` entry **is** counted, and restore skips and reports it
- [ ] A journal written before the method field existed counts 0 unprotected
- [ ] Pre-existing e2e case 8 (`captured 1 change`) still passes
- [ ] `git ls-files -z | xargs -0 tools/check-no-site-data.sh` exits 0

## Deliberately not in this plan

- Per-volume `doctor` checks and the `FICLONE` copy ladder (phase 3)
- GC accounting by method and the orphan sweep (2c)
- The cross-session evacuation gap: `rm -rf` of a store root destroys other
  sessions' backups with no record at all. This plan reports what *is*
  recorded; that gap is that nothing gets recorded in the first place, and it
  needs its own decision.

## Notes for the implementer

- **Consult the method field, not just the `-`.** A rename that overwrote
  nothing is the most common entry in a real journal and is fully restorable.
  Counting it turns the warning into noise, and a warning that cries wolf gets
  ignored exactly when it matters.
- The preview warning goes at the end of `previewSession` on purpose. That
  function is called before the confirm prompt in `cmdApply`, so ending it with
  the warning puts the warning directly above the prompt.
- `undo run` writes to stderr, not stdout, and must keep doing so: the child
  command's stdout belongs to the child.
