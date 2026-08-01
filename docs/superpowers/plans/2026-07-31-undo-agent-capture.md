# Agent Command Capture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an agent's destructive commands be captured by `undo`, so an agent
that runs `rm -rf` and realises two messages later can revert exactly that
command.

**Architecture:** Arm through the inherited environment alone — no shell hook —
and have the shim create sessions lazily, one per process group, agreed on
through an atomic symlink. Two pre-existing silent-data-loss paths that this
arming would otherwise make routine are fixed first: a journal record that
merges with the next one, and an inherited `UNDO_SESSION` that outlives its
session.

**Tech Stack:** C (`shim/undo_shim.c`, glibc, `LD_PRELOAD`), Go 1.24
(`internal/journal`, `internal/session`, `internal/restore`, `cmd/undo`), POSIX
shell hooks, bash e2e harness.

**Design:** `docs/design/undo-agent-capture-design.md`. Read it before starting;
this plan implements it and does not restate its reasoning.

## Global Constraints

- **The shim must never cause the user's command to fail.** All internal errors
  are swallowed and the real syscall's return value passed through untouched.
  Asserted by test. This plan adds many new failure paths inside the shim, so
  the assertion matters more here than usual.
- **glibc floor must stay `<= GLIBC_2.34`.** Every symbol this plan adds
  (`getsid`, `gethostname`, `sysconf`, `time`, `symlink`, `readlink`) predates
  it, but verify with `objdump` anyway after each shim task:
  ```bash
  test/in-container.sh bash -c '
    gcc -shared -fPIC -O2 -Wall -o /tmp/libundo.so shim/undo_shim.c -ldl
    objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
  ```
- **Never call `strtoul`.** Under `_GNU_SOURCE` a modern glibc redirects it to
  `__isoc23_strtoul`, which needs 2.38. Use the existing `parse_ulong`.
- **No site data in the public repo.** Run `tools/check-no-site-data.sh` and
  `tools/check-ere.sh` before every commit. Nothing is exempt.
- **Upstream `CONTRIBUTING.md` requires an e2e case for any shim change.**
  Append to `test/e2e.sh`. **The last existing case is 35**, so new cases start
  at 36.
- **Build and test only via `test/in-container.sh <cmd>`.** The shim is
  Linux-only and the workstation is macOS. Never run `make`, `go test`, `gcc`
  or `test/e2e.sh` directly.
- **macOS ships bash 3.2**: no `mapfile`, no bare `"${arr[@]}"` under `set -u`.
- **The journal format is append-only and additive.** New fields may be
  *appended* to an op, never inserted, never reordered.
- **Journal indices are load-bearing.** `restore.slot()`
  (`internal/restore/restore.go:216`) derives a filename from the entry's
  position, and the interactive picker is keyed by position. A record must
  never be dropped from the parsed entry list in a way that shifts the ones
  after it.

## File Structure

| File | Responsibility |
|---|---|
| `internal/journal/journal.go` | parse records; verify the integrity field; mark corrupt entries without dropping them |
| `internal/restore/restore.go` | refuse to act on a corrupt entry, keeping its slot |
| `internal/session/session.go` | `Pgid`/`TTL` fields, group-aware `probe`, `ttl`-first `Live`, `groups/` prune, corrupt counting |
| `shim/undo_shim.c` | integrity field, `journalv`, detach test, group identity, lazy session creation |
| `shell/undo.{bash,zsh,fish}` | export `UNDO_SID` so the detach test has a reference |
| `cmd/undo/main.go` | `undo arm`, doctor's arming section |
| `test/pathpred.c` | C unit harness — **currently orphaned; Task 2 wires it into `make test`** |
| `cmd/undo/run.go` | existing `undo run`; gains `UNDO_SID` and the `/proc` stat helpers |
| `test/e2e.sh` | cases 36 onward |
| `Makefile` | `test-c` target |
| `README.md` | `UNDO_ARM`, `UNDO_SID`, `UNDO_SESSION_MAX_AGE`, arming sites |

**Task order is deliberate.** Tasks 1–4 fix pre-existing defects and add reader
capability; nothing depends on the new arming. Tasks 5–8 build the feature on
top. A reviewer can reject any task while accepting its predecessors.

---

### Task 1: Refuse to restore a corrupt journal record

Pure reader-side. Nothing writes an integrity field yet, so every existing
journal keeps working unchanged; the tests build damaged journals by hand.

**Files:**
- Modify: `internal/journal/journal.go`
- Modify: `internal/restore/restore.go` (the `for _, i := range indices` loop)
- Modify: `internal/session/session.go` (`Unprotected`)
- Test: `internal/journal/journal_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `Entry.Corrupt bool`; `func fnv1a(s string) uint64`; the
  `journalv` sentinel filename. Task 2 writes what this reads.

- [ ] **Step 1: Write the failing test**

Add to `internal/journal/journal_test.go`:

```go
// writeJournal builds a session directory holding exactly these lines, and
// a journalv file when verify is set.
func writeJournal(t *testing.T, verify bool, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "journal"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if verify {
		if err := os.WriteFile(filepath.Join(dir, "journalv"), []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "journal")
}

// stamp returns the line with the integrity field the shim would append.
func stamp(body string) string {
	return body + "\t" + fmt.Sprintf("~%016x", fnv1a(body))
}

// The reproduced defect: a short write leaves a record unterminated, the next
// record concatenates onto it, and the merged line parses as one valid-looking
// entry whose path and backup both belong to neither record.
func TestMergedRecordIsRejected(t *testing.T) {
	merged := "unlink\t/path/to/fi" + stamp("unlink\t/other\t/bak\tlink")
	entries, err := Read(writeJournal(t, true, merged))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if !entries[0].Corrupt {
		t.Errorf("a merged record was accepted: %v; restore would write %q to %q",
			entries[0], entries[0].Backup(), entries[0].Fields)
	}
}

// A record truncated before its integrity field must not pass as a record
// written by an older shim. This is the downgrade hole: inferring "legacy"
// from a missing marker reopens exactly the corruption the marker exists to
// catch.
func TestTruncatedRecordIsNotMistakenForLegacy(t *testing.T) {
	entries, err := Read(writeJournal(t, true, "unlink\t/path\t/ba"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Corrupt {
		t.Errorf("a record truncated before its integrity field was accepted: %v", entries)
	}
}

// A session with no journalv predates the field entirely and must read exactly
// as it does today, or a rollout strands every journal already on disk.
func TestLegacyJournalReadsUnchanged(t *testing.T) {
	entries, err := Read(writeJournal(t, false, "unlink\t/path\t/bak\tlink"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Corrupt {
		t.Fatalf("legacy journal not read cleanly: %v", entries)
	}
	if got := entries[0].Backup(); got != "/bak" {
		t.Errorf("Backup() = %q, want /bak", got)
	}
	if got := entries[0].Method(); got != "link" {
		t.Errorf("Method() = %q, want link", got)
	}
}

// A well-formed stamped record must be indistinguishable from a legacy one
// once the field is stripped, or every accessor keyed to a field index breaks.
func TestStampedRecordStripsToTheSameFields(t *testing.T) {
	entries, err := Read(writeJournal(t, true, stamp("unlink\t/path\t/bak\tlink")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Corrupt {
		t.Fatalf("a valid stamped record was rejected: %v", entries)
	}
	if got := entries[0].Backup(); got != "/bak" {
		t.Errorf("Backup() = %q, want /bak", got)
	}
	if got := entries[0].Method(); got != "link" {
		t.Errorf("Method() = %q, want link", got)
	}
}

// Rejection must not be filtering. restore.slot() derives a backup filename
// from the entry's position, so dropping a corrupt record would renumber every
// entry after it and make a later restore read the wrong parked file.
func TestCorruptRecordKeepsItsSlot(t *testing.T) {
	entries, err := Read(writeJournal(t, true,
		stamp("unlink\t/a\t/bak-a\tlink"),
		"unlink\t/damaged",
		stamp("unlink\t/c\t/bak-c\tlink"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 slots, got %d: a corrupt record was dropped and every "+
			"later index shifted", len(entries))
	}
	if entries[1].Corrupt == false {
		t.Error("the damaged record was accepted")
	}
	if got := entries[2].Fields[0]; got != "/c" {
		t.Errorf("entry 2 is %q, want /c", got)
	}
}

// A short write can cut a record before its first tab, leaving a line that
// does not even look like a record. In a versioned journal it is still a slot.
// Raised by the gate: the test above uses a damaged line that happens to
// contain a tab, so it passes even when this case is dropped.
func TestShortCorruptLineStillOccupiesASlot(t *testing.T) {
	entries, err := Read(writeJournal(t, true,
		stamp("unlink\t/a\t/bak-a\tlink"),
		"unli",
		stamp("unlink\t/c\t/bak-c\tlink"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 slots, got %d: a line too short to hold a tab was "+
			"dropped and every later index shifted", len(entries))
	}
	if !entries[1].Corrupt {
		t.Error("the short line was not marked corrupt")
	}
	if got := entries[2].Fields[0]; got != "/c" {
		t.Errorf("entry 2 is %q, want /c", got)
	}
}

// The other half of that rule: a legacy journal must keep skipping short
// lines. Retaining them there would renumber every journal already on disk,
// which is the same index shift in the other direction.
func TestLegacyJournalStillSkipsShortLines(t *testing.T) {
	entries, err := Read(writeJournal(t, false,
		"unlink\t/a\t/bak-a\tlink",
		"unli",
		"unlink\t/c\t/bak-c\tlink",
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: legacy skipping changed, which "+
			"renumbers journals already on disk", len(entries))
	}
}
```

Add `"fmt"`, `"os"`, `"path/filepath"` and `"testing"` to the test file's
imports if not already present.

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/journal/ -run 'Merged|Truncated|Legacy|Stamped|Corrupt' -v`

Expected: the package does not build — `undefined: fnv1a`, and `Entry` has no
field `Corrupt`. That is the honest failure: the tests name symbols Step 3
introduces. Step 6 is where they are shown to discriminate.

- [ ] **Step 3: Implement the reader**

In `internal/journal/journal.go`, add `"path/filepath"` and `"strconv"` to the
imports.

Add the `Corrupt` field to `Entry`:

```go
type Entry struct {
	Op      string
	Fields  []string
	Corrupt bool // failed its integrity check; keeps its slot, never acted on
}
```

Add above `Read`:

```go
// integrityLen is the width of the trailing integrity field: '~' and sixteen
// hex digits.
const integrityLen = 17

// versionFile sits beside a journal and declares that every record in it
// carries an integrity field.
//
// Per journal rather than per record, and that is the whole point. Inferring
// it -- "no trailing marker means this record predates the field" -- accepts a
// record that a short write truncated before its field, which is precisely the
// corruption the field exists to catch. A truncated record cannot erase this
// file: it is written once at session setup, not per record.
const versionFile = "journalv"

// fnv1a is the 64-bit FNV-1a the shim writes over each encoded record.
//
// Non-cryptographic on purpose: this detects a truncated record and the
// concatenation of two records, not tampering. An attacker who can write the
// journal can write anything in it.
//
// The offset basis is 14695981039346656037, the real FNV-1a 64 constant. Do
// not copy it from the shim's path_hash, which uses 1469598103934665603 -- one
// digit short, harmless for a hash table's slot index and wrong here, and a
// mismatch means every record the shim writes is rejected as corrupt.
func fnv1a(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// validIntegrity reports whether field is the integrity field for body.
func validIntegrity(field, body string) bool {
	if len(field) != integrityLen || field[0] != '~' {
		return false
	}
	want, err := strconv.ParseUint(field[1:], 16, 64)
	if err != nil {
		return false
	}
	return want == fnv1a(body)
}

// parseLine turns one journal line into an entry.
//
// The bool is false only for a line that is not a record at all, and what
// qualifies differs by journal:
//
// In a versioned journal that means an empty line and nothing else. Every
// other non-empty line was written by a shim that promised an integrity field,
// so a line too short even to hold a tab is a record a short write cut off
// mid-first-field -- damaged, not absent. Dropping it would shift every index
// after it, and slot() is keyed by position, so later valid entries would
// resolve to the wrong parked files.
//
// A legacy journal keeps the historical skip of any line with fewer than two
// fields. There the shim promised nothing, and changing the rule would
// renumber journals already on disk.
func parseLine(line string, verify bool) (Entry, bool) {
	parts := strings.Split(line, "\t")
	if verify {
		if line == "" {
			return Entry{}, false
		}
		// Op is kept so the entry describes itself in a listing; the fields
		// are deliberately not, because they are the untrustworthy part and
		// nothing should be tempted to read them.
		corrupt := Entry{Op: decode(parts[0]), Corrupt: true}
		if len(parts) < 2 {
			return corrupt, true
		}
		last := parts[len(parts)-1]
		// everything before the final tab, which is what the shim hashed
		body := line[:len(line)-len(last)-1]
		if !validIntegrity(last, body) {
			return corrupt, true
		}
		parts = parts[:len(parts)-1]
	} else if len(parts) < 2 {
		return Entry{}, false
	}
	e := Entry{Op: decode(parts[0])}
	for _, p := range parts[1:] {
		e.Fields = append(e.Fields, decode(p))
	}
	return e, true
}
```

Replace the body of `Read`'s scan loop:

```go
	// Whether records must validate is declared by the session, not guessed
	// per record. See versionFile.
	_, verr := os.Stat(filepath.Join(filepath.Dir(path), versionFile))
	verify := verr == nil

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		e, ok := parseLine(sc.Text(), verify)
		if !ok {
			continue
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
```

Add to `Describe`, as the first case in the switch:

```go
	if e.Corrupt {
		return "CORRUPT   " + e.Op + " record (integrity check failed)"
	}
```

placed immediately after the `f := func(i int) string { ... }` closure and
before `switch e.Op {`.

- [ ] **Step 4: Make restore refuse a corrupt entry**

In `internal/restore/restore.go`, inside `for _, i := range indices`, directly
after the `skip := func(why string) { ... }` definition and before
`act := func() bool`:

```go
		// A corrupt record's fields are whatever survived the damage. Acting on
		// them means restoring the wrong backup over the wrong path, silently,
		// which is the failure the integrity field exists to prevent. The slot
		// is still consumed: slot() is keyed by position.
		if e.Corrupt {
			skip("record failed its integrity check")
			continue
		}
```

- [ ] **Step 5: Count corrupt entries as unprotected**

In `internal/session/session.go`, in `Unprotected`, as the first check inside
the loop:

```go
		if e.Corrupt {
			n++
			continue
		}
```

This is what makes `undo list` mark the session and the pre-revert preview warn
before the user relies on it.

- [ ] **Step 6: Run the tests and prove they discriminate**

Run: `test/in-container.sh go test ./internal/journal/ -v`
Expected: PASS (7 new tests).

Run: `test/in-container.sh go test ./...`
Expected: PASS. No existing journal has a `journalv` beside it, so `verify` is
false everywhere and every existing test exercises the unchanged path.

Now prove the new tests are evidence. Temporarily make `parseLine` ignore
`verify` (return the entry without checking) and rerun. Confirm
`TestMergedRecordIsRejected` and `TestTruncatedRecordIsNotMistakenForLegacy`
both FAIL. Restore.

Then temporarily change `parseLine` to `return Entry{}, false` for a corrupt
record instead of marking it, and confirm `TestCorruptRecordKeepsItsSlot`
FAILS on the slot count. Restore.

Then temporarily hoist the `len(parts) < 2` check above the `if verify` block —
which is where it sat before the gate caught it — and confirm
`TestShortCorruptLineStillOccupiesASlot` FAILS while
`TestLegacyJournalStillSkipsShortLines` still passes. That pair is what pins
the rule in both directions. Restore.

A regression test that passes with and without the fix is not evidence.

- [ ] **Step 7: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/journal/ internal/restore/restore.go internal/session/session.go
git diff --cached --stat
git commit -m "journal: refuse to restore a record that failed its integrity check"
```

---

### Task 2: Write the integrity field from the shim

**Files:**
- Modify: `shim/undo_shim.c` (`journal_fd`, `jwritev`)
- Modify: `test/pathpred.c` (extend; also fix its truncation warning)
- Modify: `Makefile` (new `test-c` target — the harness is currently orphaned)
- Modify: `test/e2e.sh` (cases 36–38)

**Interfaces:**
- Consumes: the reader from Task 1 — the `journalv` filename, the `~` +
  16-lowercase-hex field format, and FNV-1a with offset basis
  `14695981039346656037` and prime `1099511628211`.
- Produces: journals that Task 1's reader validates. Nothing later depends on it.

- [ ] **Step 1: Wire the orphaned C harness into the build**

`test/pathpred.c` exists and passes but **nothing runs it** — no Makefile
target and no script references it. Every C unit test below would be dead code
without this step.

In `Makefile`, add after the `bin/undo` rule:

```make
# The C harness includes the shim translation unit directly, which is how its
# static functions get tested without exporting them.
build/shimunit: test/pathpred.c shim/undo_shim.c
	@mkdir -p build
	$(CC) -O2 -Wall -Wextra -o $@ test/pathpred.c -ldl
```

and change the `test` target to:

```make
test: all build/shimunit
	./build/shimunit
	go test ./...
	./test/e2e.sh
```

Add `build/shimunit` to nothing else; `clean` already removes `build`.

`test/pathpred.c:51` currently warns under `-Wextra`:
`'/f.txt' directive output may be truncated`. Fix it so `make test` is quiet —
in `test/pathpred.c`, change the declaration of `file` from `char file[PATH_MAX];`
to:

```c
        char file[PATH_MAX + 16]; /* room for the suffix snprintf appends */
```

- [ ] **Step 2: Write the failing C unit test**

Append to `main()` in `test/pathpred.c`, before the final `printf`/`return`:

```c
    /* The integrity hash must be byte-identical to the Go reader's fnv1a, or
     * every record the shim writes is rejected as corrupt. These vectors are
     * the standard FNV-1a 64 results and are what internal/journal computes. */
    expect(rec_hash("", 0) == 14695981039346656037ULL, 1, "fnv1a empty");
    expect(rec_hash("a", 1) == 12638187200555641996ULL, 1, "fnv1a a");
    expect(rec_hash("foobar", 6) == 9625390261332436968ULL, 1, "fnv1a foobar");
```

- [ ] **Step 3: Run it to make sure it fails**

Run: `test/in-container.sh make build/shimunit`
Expected: FAIL to compile — `implicit declaration of function 'rec_hash'`.

- [ ] **Step 4: Implement the shim side**

In `shim/undo_shim.c`, add above `jwritev`:

```c
/* FNV-1a over an encoded record. Mirrored exactly by fnv1a in
 * internal/journal: if the two ever disagree the reader rejects every record
 * the shim writes, and undo silently restores nothing.
 *
 * Deliberately not path_hash(): that one maps 0 to 1 so zero can mark an empty
 * table slot, and replicating that quirk on the Go side for no reason is how
 * two implementations of one hash drift apart. */
static uint64_t rec_hash(const char *s, size_t len)
{
    uint64_t h = 14695981039346656037ULL;
    for (size_t i = 0; i < len; i++) {
        h ^= (unsigned char)s[i];
        h *= 1099511628211ULL;
    }
    return h;
}
```

**The offset basis is `14695981039346656037ULL`, not `path_hash`'s
`1469598103934665603ULL` sitting a few hundred lines below.** The latter is one
digit short — fine for choosing a hash-table slot, wrong as FNV-1a. Copying it
here makes every record the shim writes fail the Go reader's check, and case 36
is what catches that.

Replace `jwritev`'s body from the `line` declaration to the `write` call. The
encoding loop changes too — it must encode into a **reduced** capacity, not the
whole buffer:

```c
    char line[4 * PATH_MAX];
    /* Reserve the tail for the integrity field and the newline so encoding can
     * never consume the room they need.
     *
     * Stamping only "if it fits" is what this replaces, and it was wrong in the
     * worst direction: a record written without its stamp into a journal whose
     * journalv promises every record has one is read as corrupt, and the change
     * it records becomes unrestorable. A full buffer would have silently
     * disarmed the guarantee this field exists to provide.
     *
     * enc_append still truncates a field that does not fit in what remains.
     * That is pre-existing and bounded -- the buffer is 4 * PATH_MAX and the
     * widest record the shim writes is an op and three paths -- so it is left
     * alone here rather than fixed halfway. */
    const size_t cap = sizeof line - integrity_width - 1;
    size_t len = 0;
    enc_append(line, cap, &len, op);
    const char *f;
    while ((f = va_arg(ap, const char *)) != NULL) {
        if (len + 1 < cap)
            line[len++] = '\t';
        enc_append(line, cap, &len, f);
    }
    /* Hashed over everything preceding it, which is exactly what the reader
     * recomputes from the text before the final tab. Both writes below fit by
     * construction: cap left integrity_width + 1 bytes free. */
    uint64_t h = rec_hash(line, len);
    int n = snprintf(line + len, sizeof line - len, "\t~%016llx",
                     (unsigned long long)h);
    if (n > 0)
        len += (size_t)n;
    line[len++] = '\n';
    ssize_t r = write(fd, line, len);
    /* A short write leaves a record with no newline, so the next record
     * concatenates onto it and is read as one entry with the wrong path and
     * the wrong backup. Terminate it.
     *
     * This only covers a single writer. A concurrent member of the same
     * process group can land a complete record in the gap between these two
     * writes, and the merge happens anyway -- which is why the integrity field
     * above is what correctness actually rests on, and this is only
     * containment. Every writer-side remedy asks a failing writer to perform
     * more successful I/O at the moment I/O is failing. */
    if (r >= 0 && (size_t)r < len) {
        ssize_t nl = write(fd, "\n", 1);
        (void)nl;
    }
```

and add near `DEFAULT_MAX_BYTES`:

```c
/* What the integrity field costs at the end of a record: '\t', '~', sixteen
 * hex digits and the NUL snprintf writes is 19; the twentieth byte is slack so
 * the reservation in jwritev is provably sufficient rather than exactly so. */
#define integrity_width 20
```

Add above `journal_fd`:

```c
/* Opens a session's journal, declaring the integrity contract if and only if
 * this call is what brought the journal into existence.
 *
 * O_EXCL is the whole mechanism. Exactly one process can create the file, and
 * only that process writes journalv, so the decision needs no other
 * coordination.
 *
 * Deciding from the journal's size instead would race, which is how this was
 * first written: a peer can append between another process's open and its
 * stat, and both then conclude someone else must have declared it. The session
 * is read as legacy forever and integrity checking is silently off for it.
 *
 * A journal that already exists is never versioned here. It was either created
 * by a peer, which declared the contract, or written by an older shim, in
 * which case declaring it now would mark every record that shim wrote corrupt
 * and make a session unrestorable the moment the shim is upgraded under it. */
static int open_journal(const char *dir, const char *path)
{
    REAL(open, int, const char *, int, ...);
    int fd = real_open(path, O_WRONLY | O_APPEND | O_CREAT | O_EXCL | O_CLOEXEC,
                       0600);
    if (fd < 0)
        return real_open(path, O_WRONLY | O_APPEND | O_CLOEXEC, 0600);
    char vpath[PATH_MAX];
    if ((size_t)snprintf(vpath, sizeof vpath, "%s/journalv", dir) <
        sizeof vpath) {
        int vfd = real_open(vpath, O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC,
                            0600);
        if (vfd >= 0) {
            ssize_t vr = write(vfd, "1\n", 2);
            (void)vr;
            close(vfd);
        }
    }
    return fd;
}
```

In `journal_fd`, replace the success arm:

```c
    fd = open_journal(dir, path);
    if (fd >= 0)
        snprintf(cached_dir, sizeof cached_dir, "%s", dir);
    return fd;
```

And in `other_journal_fd`, replace its final `real_open` with the same helper,
so reporting a loss into a session that has not journaled anything yet does not
leave that session an unversioned journal full of stamped records:

```c
    return open_journal(sdir, path);
```

- [ ] **Step 5: Add the e2e cases**

Add these helpers near `fail()` in `test/e2e.sh` first — every case below uses
them, and hand-rolling the field reads is what the gate caught twice:

```bash
# Field <n> of /proc/<pid>/stat, one-indexed.
#
# Everything up to the LAST ')' is stripped first: field 2 is the executable
# name in parentheses and may contain spaces and parentheses of its own, so
# `cut -d' ' -fN` silently reads the wrong field for such a process.
statfield() { # statfield <pid|self> <n>
    sed 's/.*) //' "/proc/$1/stat" | awk -v n="$(( $2 - 2 ))" '{print $n}'
}

# UNDO_ARM exactly as `undo arm` builds it: the process group's id, paired with
# the START TIME OF THE GROUP LEADER -- not our own.
#
# When this shell is not its group leader, which is the ordinary case in a
# container with no job control, those are two different processes and pairing
# them describes nothing at all. armer_is_us would never match and the
# exclusion would silently stop working.
#
# Falls back to the static "1" exactly as cmdArm does, rather than failing.
# A group whose leader has already exited has no /proc/<pgid>/stat, and
# returning nothing would expand to UNDO_ARM= -- an empty value, which is a
# third state neither the shim nor this harness should have to reason about.
arm_id() {
    local pgid st
    pgid=$(statfield self 5)
    st=""
    [[ -n $pgid ]] && st=$(statfield "$pgid" 22)
    if [[ -z $pgid || -z $st ]]; then
        printf '1\n'
        return 0
    fi
    printf '%s:%s\n' "$pgid" "$st"
}
```

**Then fix case 25, which this task breaks.** It is the only assertion in the
suite anchored to the END of a journal record:

```bash
grep -q $'\tlink$' "$sess/journal" ||
    fail "the unlink record does not end with the save method"
```

Appending an integrity field makes the method no longer last, so it fails.
Replace it with a positional check, matching what every other assertion in the
file already does (line 340 immediately below, and `test/multifs-store.sh:44`)
and which no future trailing field can break:

```bash
awk -F'\t' '$1=="unlink" && $4=="link"' "$sess/journal" | grep -q . ||
    fail "the unlink record does not name link as its save method"
```

Found during execution, not by review. The design's claim that the format is
"additive, and `journal.Read` already tolerates trailing fields" was verified
against `journal.go`, `restore.go` and `session.go` — and not against the shell
tests, which is half the surface. Nothing else in `test/*.sh` is end-anchored;
that was checked afterwards rather than assumed.

Then append the cases:

```bash
echo "== case 36: every record carries an integrity field the reader accepts"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
[[ -f $sess/journalv ]] || fail "case 36: no journalv beside the journal"
while IFS= read -r line; do
    [[ $line == *$'\t'~* ]] || fail "case 36: record has no integrity field: $line"
done <"$sess/journal"
# the whole point: a stamped journal still restores
"$UNDO" -y
[[ -f $PLAY/top.txt ]] || fail "case 36: a stamped journal did not restore"

echo "== case 37: a merged record is refused, not restored onto the wrong path"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
# what a short write leaves behind: a record with no newline, then the next one
{ printf 'unlink\t%s/docs/report.txt' "$PLAY"; cat "$sess/journal"; } >"$sess/j2"
mv "$sess/j2" "$sess/journal"
rm -f "$PLAY/top.txt"
"$UNDO" -y >/dev/null 2>&1 || true
[[ $(cat "$PLAY/docs/report.txt") == "report v1" ]] ||
    fail "case 37: a merged record restored a backup over the wrong path"

echo "== case 38: upgrading the shim under a session in flight keeps it restorable"
# A session an older shim already wrote unstamped records into. Declaring the
# integrity contract over those would mark every one of them corrupt, and the
# session would stop being restorable the moment the shim was upgraded.
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "a session started before the shim was upgraded" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
cp "$PLAY/top.txt" "$sess/data/legacy-1"
rm -f "$PLAY/top.txt"
printf 'unlink\t%s\t%s\tlink\n' "$PLAY/top.txt" "$sess/data/legacy-1" >"$sess/journal"
# now the new shim appends to that same journal
env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
    LD_PRELOAD="$LIB" bash -c "rm $PLAY/docs/report.txt"
[[ ! -f $sess/journalv ]] ||
    fail "case 38: the integrity contract was declared over records written \
before it existed; every one of them now reads as corrupt"
"$UNDO" apply "$id" -y >/dev/null
[[ -f $PLAY/top.txt ]] ||
    fail "case 38: a record written by the older shim was not restored"
```



- [ ] **Step 6: Run everything**

Run: `test/in-container.sh make test`
Expected: PASS, including `./build/shimunit` and cases 36–38.

If the C and Go hashes disagree, case 36 fails with every record rejected.
Check the offset basis first — see the warning in Step 4.

Run the floor check from Global Constraints.
Expected: `GLIBC_2.34`. `snprintf` and `write` are already used; nothing new.

- [ ] **Step 7: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add shim/undo_shim.c test/pathpred.c Makefile test/e2e.sh
git diff --cached --stat
git commit -m "shim: stamp every journal record so a merged one cannot be restored"
```

---

### Task 3: Liveness by process group and by ttl

Pure reader capability. Nothing writes `pgid` or `ttl` until Task 6, so every
existing session keeps today's behaviour exactly; the tests build sessions with
those files by hand.

**Files:**
- Modify: `internal/session/session.go` (`Session`, `load`, `Live`, `probe`)
- Test: `internal/session/session_test.go`

**Interfaces:**
- Consumes: `thisHost()`, `withinGrace()`, `foreignGrace()` — all present.
- Produces: `Session.Pgid int`, `Session.TTL time.Time`, `const ttlSkew`.
  Task 6 writes the files these read.

- [ ] **Step 1: Write the failing test**

Add to `internal/session/session_test.go`:

```go
// shimSession builds a session as the shim will write one: a recorded process
// group and a ttl, with our own origin so the local path is exercised.
func shimSession(t *testing.T, pgid int, ttl time.Time) *Session {
	t.Helper()
	s, err := Create("rm -rf x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "pgid"),
		[]byte(strconv.Itoa(pgid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "ttl"),
		[]byte(strconv.FormatInt(ttl.Unix(), 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// liveGroup starts a helper process in a process group of its own and returns
// that group's id.
//
// A test cannot use its own process group for this. Under `make test` in a
// container there is no job control and everything shares pgid 1 -- measured,
// not assumed -- which is precisely the value probe() must refuse to hand to
// kill(-pgid, 0). A test written with syscall.Getpgrp() therefore exercises
// the pid path it was meant to avoid, and fails.
//
// Setpgid makes the child a group leader, so its pid is also its pgid.
func liveGroup(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// The defect a leader-pid probe reintroduces: the group leader exits while a
// child keeps writing, the probe says finished, and gc deletes the backups of
// a command that is still running.
func TestLiveProbesTheGroupNotTheLeader(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	// our own process group: certainly alive, and its leader may not be us
	s := shimSession(t, liveGroup(t), time.Now().Add(time.Hour))
	// a leader pid that is not running, as if the leader had already exited
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("a session whose group is alive read as finished because its " +
			"recorded leader had exited; gc would delete a running command's backups")
	}
}

// kill(-1, 0) is the "every process you may signal" form: it succeeds always.
// A session recording pgid 1 -- a container with no job control -- must not be
// probed that way, or it is pinned live forever and nothing can collect it.
func TestLiveDoesNotProbeGroupOneAsAWildcard(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := shimSession(t, 1, time.Now().Add(time.Hour))
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("pgid 1 was probed as kill(-1, 0), which always succeeds; the " +
			"session is now uncollectable")
	}
}

// A rolled bucket is retired by its own ttl, with nobody having written a done
// marker. Without this a daemon holds one session whose group never dies, and
// gc can never collect it.
func TestLiveRetiresASessionPastItsTTL(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := shimSession(t, liveGroup(t), time.Now().Add(-time.Hour))
	if s.Live() {
		t.Error("a session past its ttl read as live even though its group is " +
			"still running; a long-lived process would pin it forever")
	}
}

// The skew allowance exists because the ttl is one node's clock read from
// another's. Just past the instant is not past it.
func TestLiveHonoursTheSkewAllowance(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := shimSession(t, liveGroup(t), time.Now().Add(-time.Minute))
	if !s.Live() {
		t.Error("a session one minute past its ttl was retired; clock skew " +
			"between nodes would collect commands that just started")
	}
}

// ttl is authoritative from any node, unlike a pid. Otherwise a foreign
// session is held for the whole foreign grace and the bound does not work
// wherever gc happens to run.
func TestLiveRetiresAForeignSessionPastItsTTL(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := shimSession(t, 2147483647, time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(s.Dir, "host"),
		[]byte("othernode\tnot-our-boot-id\tpid:[1]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("a foreign session past its ttl was held by the foreign grace; " +
			"the long-lived-group bound then depends on which node runs gc")
	}
}

// A foreign session inside its ttl is presumed running, and the ttl is a
// tighter bound than the week-long grace.
func TestLiveHoldsAForeignSessionInsideItsTTL(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := shimSession(t, 2147483647, time.Now().Add(time.Hour))
	if err := os.WriteFile(filepath.Join(s.Dir, "host"),
		[]byte("othernode\tnot-our-boot-id\tpid:[1]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("a foreign session inside its ttl was called finished")
	}
}

// Sessions written before this change have neither file and must keep the old
// behaviour exactly, or a rollout strands what is already on disk.
func TestLiveWithoutPgidOrTTLIsUnchanged(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	if s.Pgid != 0 || !s.TTL.IsZero() {
		t.Fatalf("setup: want no pgid and no ttl, got %d %v", s.Pgid, s.TTL)
	}
	got, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Live() {
		t.Error("our own running pid stopped reading as live")
	}
}
```

Add `"os/exec"` and `"syscall"` to the test file's imports if not already
present.

**Do not reach for `syscall.Getpgrp()` as "a group that is certainly alive".**
Found during execution: under `make test` in a container there is no job
control and every process shares pgid 1, which is exactly the value `probe()`
refuses to pass to `kill(-1, 0)`. A test written that way silently exercises
the pid path instead of the group path. Note that Step 5's mutation check
would NOT have caught this: it confirms a test can fail, not that its setup
means what you think it does.

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/session/ -run 'TestLive' -v`
Expected: the package does not build — `Session` has no field `Pgid` or `TTL`,
and `ttlSkew` is undefined.

- [ ] **Step 3: Implement**

In `internal/session/session.go`, extend the package doc block after the `host`
entry:

```go
//	pgid     - the process group the command ran in, when the shim created the
//	           session. The group is what is alive; the leader may exit while a
//	           child keeps writing. See Live.
//	ttl      - unix seconds at which this session stops being presumed to be
//	           running. Unlike a pid it is meaningful from any node.
```

Add to the `Session` struct, after `Origin`:

```go
	Pgid     int       // process group, 0 when none was recorded
	TTL      time.Time // when this session stops being presumed live, zero for none
```

In `load()`, after the `host` block:

```go
	if b, err := os.ReadFile(filepath.Join(dir, "pgid")); err == nil {
		s.Pgid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	// Unix seconds rather than a formatted timestamp: the shim writes this,
	// and formatting a time in C costs code and a symbol it needs for nothing
	// else.
	if b, err := os.ReadFile(filepath.Join(dir, "ttl")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 {
			s.TTL = time.Unix(n, 0)
		}
	}
```

Add near `minForeignGrace`:

```go
// ttlSkew allows for the ttl having been written by one node's clock and read
// by another's. Retiring a session early does not lose the command's data, but
// it does lose the ability to undo it, so the allowance errs long.
const ttlSkew = 5 * time.Minute
```

Replace `Live` and `probe`:

```go
// Live reports whether the session's command may still be running.
//
// The order matters and is not arbitrary:
//
//  1. A done marker is conclusive.
//  2. A recorded ttl is authoritative from any node. Unlike a pid it names an
//     absolute instant, so it means the same thing read from anywhere -- which
//     is what retires a rolled bucket even when gc only ever runs elsewhere,
//     and what stops a long-lived process group from holding one session that
//     nothing can ever collect.
//  3. A session from another origin cannot be probed at all. Inside its ttl it
//     is presumed running; with no ttl it falls back to the foreign grace,
//     exactly as before.
//  4. Otherwise probe, the group when one was recorded.
func (s *Session) Live() bool {
	if s.Done {
		return false
	}
	if !s.TTL.IsZero() && time.Now().After(s.TTL.Add(ttlSkew)) {
		return false
	}
	// An empty local identity means we could not establish which kernel
	// instance we are, so nothing can be classified and neither signal is
	// sound alone.
	local := thisHost()
	if local == "" {
		return s.probe() || s.withinGrace()
	}
	if s.Origin != "" && s.Origin != local {
		if !s.TTL.IsZero() {
			return true // inside its ttl, and step 2 already ruled out past it
		}
		return s.withinGrace()
	}
	return s.probe()
}

// probe asks the kernel whether this session's creator is still around. Only
// meaningful for a session issued by this kernel instance.
//
// A process group when one was recorded: the group is what the session belongs
// to. Probing the leader alone is wrong the moment the leader exits while a
// child keeps writing, and being wrong there means gc deletes the backups of a
// command that is still running.
//
// The Pgid > 1 guard is load-bearing, not defensive. kill(-1, 0) is the "every
// process you may signal" form; it succeeds always, so a session recording
// pgid 1 would read live forever and nothing could ever collect it.
func (s *Session) probe() bool {
	if s.Pgid > 1 {
		err := syscall.Kill(-s.Pgid, 0)
		return err == nil || err == syscall.EPERM
	}
	if s.Pid <= 0 {
		return false
	}
	err := syscall.Kill(s.Pid, 0)
	return err == nil || err == syscall.EPERM
}
```

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ -v`
Expected: PASS (7 new tests plus everything already there).

Run: `test/in-container.sh make test`
Expected: PASS. No session on disk has `pgid` or `ttl` yet, so every existing
case takes the unchanged path.

- [ ] **Step 5: Prove the new tests discriminate**

Temporarily drop the `s.Pgid > 1` branch from `probe` and rerun. Confirm
`TestLiveProbesTheGroupNotTheLeader` FAILS. Restore.

Then change the guard to `s.Pgid >= 1` and confirm
`TestLiveDoesNotProbeGroupOneAsAWildcard` FAILS on its own. Restore.

Then drop the ttl check from `Live` and confirm both
`TestLiveRetiresASessionPastItsTTL` and
`TestLiveRetiresAForeignSessionPastItsTTL` FAIL. Restore.

- [ ] **Step 6: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/
git diff --cached --stat
git commit -m "session: the process group is what is alive, and a ttl says when it stops"
```

---

### Task 4: The detach test

Closes a pre-existing leak: a process that inherits `UNDO_SESSION` and then
`setsid()`s away — a terminal multiplexer server is the common case — hands
that stale session to every child for its whole life, and those children end up
appending to an unlinked inode after gc collects it.

**Files:**
- Modify: `shim/undo_shim.c` (`session_dir`)
- Modify: `cmd/undo/run.go` (`armedEnv`, new `procStatField`/`selfStatField`)
- Modify: `shell/undo.bash`, `shell/undo.zsh`, `shell/undo.fish`
- Modify: `test/e2e.sh` (cases 39–40)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: the `UNDO_SID` contract — the terminal session id, decimal, set by
  whoever sets `UNDO_SESSION`. Task 7's `undo arm` sets it too.

- [ ] **Step 1: Write the failing e2e cases**

Append to `test/e2e.sh`:

```bash
echo "== case 39: a process that detached ignores the session it inherited"
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "the command that started the daemon" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
# setsid puts the command in a new terminal session, which is what a
# multiplexer server or any daemonizing program does
env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
    LD_PRELOAD="$LIB" setsid bash -c "rm $PLAY/top.txt" || true
[[ ! -e $PLAY/top.txt ]] || fail "case 39: the rm did not run"
[[ ! -s $sess/journal ]] ||
    fail "case 39: a detached process wrote into the session it inherited; \
after gc collects that session those writes go to an unlinked inode"

echo "== case 40: without UNDO_SID the inherited session is still honoured"
make_tree
id=$(date +%s%N | cut -c1-16)
sess=$UNDO_DATA_DIR/sessions/$id
mkdir -p "$sess/data"
echo "an older hook that does not set UNDO_SID" >"$sess/cmd"
printf '%s\t%s\t%s\n' "$(uname -n)" \
    "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" \
    "$(readlink /proc/self/ns/pid 2>/dev/null)" >"$sess/host"
env UNDO_SESSION="$sess" LD_PRELOAD="$LIB" setsid bash -c "rm $PLAY/top.txt" || true
[[ -s $sess/journal ]] ||
    fail "case 40: the detach test disarmed a shell whose hook predates \
UNDO_SID; a rollout would silently stop recording"
```

- [ ] **Step 2: Run them to make sure case 39 fails**

Run: `test/in-container.sh bash test/e2e.sh`
Expected: FAIL at case 39 — the detached process wrote into the inherited
session. Case 40 should PASS already: it pins behaviour that must not move.

- [ ] **Step 3: Implement the shim side**

In `shim/undo_shim.c`, replace `session_dir`:

```c
/* The session this process should write to, or NULL.
 *
 * An inherited UNDO_SESSION is rejected when this process has left the
 * terminal session that set it. A process that calls setsid() -- a terminal
 * multiplexer server, any daemonizing program -- carries the variable for its
 * entire life and hands it to every child it later spawns; those children then
 * append to a session that finished long ago, and once gc collects it, to an
 * unlinked inode, with nothing printed.
 *
 * Job control changes a child's pgid but not its sid, so an ordinary command
 * from a hooked shell still matches and nothing about the existing hook path
 * changes.
 *
 * Deliberately conservative: with UNDO_SID unset there is no trustworthy
 * reference, so the variable is honoured exactly as before. That is what keeps
 * sessions from an older hook working through a rollout instead of silently
 * disarming them. */
static const char *session_dir(void)
{
    const char *s = getenv("UNDO_SESSION");
    if (!s || !*s || *s != '/')
        return NULL;
    const char *sid = getenv("UNDO_SID");
    if (sid && *sid) {
        unsigned long armed_sid = parse_ulong(sid);
        if (armed_sid != 0 && (unsigned long)getsid(0) != armed_sid)
            return NULL;
    }
    return s;
}
```

- [ ] **Step 4: Export `UNDO_SID` from every setter of `UNDO_SESSION`**

There are four: the three shell hooks and `undo run`. A setter that omits it
leaves the detach test with no reference, so an inherited session is trusted
unconditionally there — the defect narrowed rather than fixed.

**`undo run` first**, since it is the one in Go. In `cmd/undo/run.go`, extend
`armedEnv`'s strip list so a stale value cannot survive:

```go
		if strings.HasPrefix(kv, "UNDO_SESSION=") ||
			strings.HasPrefix(kv, "UNDO_SID=") {
			continue
		}
```

and replace its return:

```go
	env = append(env, "UNDO_SESSION="+dir, "LD_PRELOAD="+preload)
	// Whoever sets UNDO_SESSION sets UNDO_SID. Without it a daemon this
	// command starts inherits the session for its entire life and keeps
	// appending to it long after it finished -- and once gc collects it, to an
	// unlinked inode, with nothing printed. See session_dir in the shim.
	if sid := selfStatField(6); sid != "" {
		env = append(env, "UNDO_SID="+sid)
	}
	return env
```

Add the helper to `cmd/undo/run.go`; Task 7 reuses it rather than redefining it:

```go
// procStatField reads a one-indexed space-separated field of a /proc stat file.
//
// Counted forward from the last ')': field 2 is the executable name in
// parentheses and may itself contain spaces and parentheses, which is the
// classic way to misparse this file.
func procStatField(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3, so field n is at index n-3
	if n < 3 || n-3 >= len(f) {
		return ""
	}
	return f[n-3]
}

func selfStatField(n int) string { return procStatField("/proc/self/stat", n) }
```

**Then the three hooks.** The value is read once at source time: a shell does
not change its terminal session id while it lives, and field 6 of
`/proc/self/stat` costs no
subprocess. Field 2 is the executable name in parentheses and can contain
spaces, but a shell's name cannot, so a positional read is safe here.

In `shell/undo.zsh`, after the `_undo_origin` block and before the
`unset _undo_boot _undo_pidns` line, add `_undo_sid` to that unset and insert:

```zsh
# The terminal session id, so the shim can tell an inherited UNDO_SESSION from
# one meant for this process. See session_dir in the shim.
_undo_sid=
if [[ -r /proc/self/stat ]]; then
    read -r _ _ _ _ _ _undo_sid _ < /proc/self/stat
fi
[[ -n $_undo_sid ]] && export UNDO_SID=$_undo_sid
```

In `shell/undo.bash`, in the same position relative to its `_undo_origin`
block:

```bash
_undo_sid=
if [[ -r /proc/self/stat ]]; then
    read -r _ _ _ _ _ _undo_sid _ < /proc/self/stat
fi
[[ -n $_undo_sid ]] && export UNDO_SID=$_undo_sid
unset _undo_sid
```

In `shell/undo.fish`, after its `_undo_origin` block:

```fish
# see session_dir in the shim
if test -r /proc/self/stat
    set -l _undo_stat (string split ' ' < /proc/self/stat)
    if test (count $_undo_stat) -ge 6
        set -gx UNDO_SID $_undo_stat[6]
    end
end
```

**Each hook must drop a mismatched inherited session BEFORE publishing its own
sid.** Found by the gate after this task first shipped, and it defeats the
whole change if omitted: a multiplexer pane inherits UNDO_SESSION, UNDO_SID and
LD_PRELOAD, the shim correctly rejects the stale session while UNDO_SID still
names the terminal session that set it -- and then the hook exports UNDO_SID
with its own sid, the comparison matches again, and the stale session is live
for the rest of shell startup.

In `shell/undo.bash` and `shell/undo.zsh`, immediately before the
`export UNDO_SID` line:

```bash
# A session inherited across setsid is not ours. We are about to overwrite
# UNDO_SID with our own sid, which would re-authorise it for the rest of
# startup, so it goes first. Only on disagreement: unset UNDO_SID means no
# reference, and the shim honours UNDO_SESSION in that case by design. The
# same applies when we could not read our own sid: unknown means do not
# act, or a nested shell sharing its parent's session is disarmed for the
# whole of startup.
if [[ -n $_undo_sid && -n ${UNDO_SESSION-} && -n ${UNDO_SID-} && ${UNDO_SID-} != "$_undo_sid" ]]; then
    unset UNDO_SESSION
fi
```


**An unreadable input is not a mismatch.** The `-n $_undo_sid` term is the
whole point of that first condition: without it, a shell that cannot read
`/proc/self/stat` compares any inherited `UNDO_SID` against the empty string,
finds them unequal, and disarms a session it had no evidence was stale. The
shim resolves the same doubt the other way -- unset `UNDO_SID` means honour
`UNDO_SESSION` -- and the two must agree or they disagree about the same
uncertainty. `shell/undo.fish` needs no equivalent because its guard already
sits inside the block that runs only when the stat file parsed.

In `shell/undo.fish`, immediately before `set -gx UNDO_SID $_undo_stat[6]`:

```fish
        if set -q UNDO_SESSION; and set -q UNDO_SID
            if test "$UNDO_SID" != "$_undo_stat[6]"
                set -e UNDO_SESSION
            end
        end
```

**And pin it in `test/hook-zsh.sh`**, which is the only harness that exercises
hook *sourcing*. Add `"$WORK/zdot2"` to the existing `mkdir -p`, then before
the final success message:

```bash
# A session inherited across setsid must not survive sourcing the hook.
#
# Recorded from inside .zshrc rather than asked for afterwards: _undo_preexec
# creates a fresh session before the first command runs, so an interactive
# `echo $UNDO_SESSION` always reports that new session and can never see the
# window this guards. The first version of this test did exactly that and
# reported a real session id, which reads as the guard failing.
cat >"$WORK/zdot2/.zshrc" <<EOF
export UNDO_DATA_DIR=$WORK/store
export UNDO_LIB=$ROOT/build/libundo.so
export UNDO_SESSION=$WORK/stale-session
export UNDO_SID=999999
path=($ROOT/bin \$path)
source $ROOT/shell/undo.zsh
print -r -- "AFTER_SOURCE=[\${UNDO_SESSION-unset}]" >"$WORK/observed"
EOF
printf 'exit\n' | ZDOTDIR=$WORK/zdot2 zsh -i >/dev/null 2>&1 || true
if ! grep -q 'AFTER_SOURCE=\[unset\]' "$WORK/observed" 2>/dev/null; then
    echo "FAIL: the hook kept a session inherited across setsid" >&2
    cat "$WORK/observed" 2>/dev/null >&2
    exit 1
fi
echo "stale-session guard passed"
```

Verified to discriminate: with the guard both scenarios pass, with the guard
reverted this one fails.

- [ ] **Step 5: Make the e2e harness set it too**

`run_armed` stands in for a hook, so it must export what a hook exports or no
case exercises the matching path.

In `test/e2e.sh`, in `run_armed`, change the `env` line to:

```bash
    env UNDO_SESSION="$sess" UNDO_SID="$(statfield self 6)" \
        LD_PRELOAD="$LIB" bash -c "$*"
```

- [ ] **Step 6: Run everything**

Run: `test/in-container.sh make test`
Expected: PASS, including cases 39 and 40. Every earlier case now runs with
`UNDO_SID` set and a matching sid, which is the path a hooked shell takes.

Run: `test/in-container.sh bash -c 'apt-get install -y -qq zsh >/dev/null && make all >/dev/null && bash test/hook-zsh.sh'`
Expected: `zsh hook smoke test passed`.

**zsh is not in the test image**, so the bare
`test/in-container.sh bash test/hook-zsh.sh` this plan originally gave
exits 127 with `zsh: command not found` — an unrunnable check that reads
as a failure rather than as a skip. Found during execution. Installing it
first is what makes this step verify anything, and it matters here
because this task changes `shell/undo.zsh`.

Run the floor check.
Expected: `GLIBC_2.34`. `getsid` is `GLIBC_2.2.5`.

- [ ] **Step 7: Prove case 39 discriminates**

Temporarily revert the `UNDO_SID` branch in `session_dir` and rerun
`test/in-container.sh bash test/e2e.sh`. Confirm case 39 FAILS and case 40
still passes. Restore.

- [ ] **Step 8: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add shim/undo_shim.c cmd/undo/run.go shell/ test/e2e.sh
git diff --cached --stat
git commit -m "shim: a session inherited across setsid is not this process's session"
```

---

### Task 5: Group identity

Pure functions plus their unit tests. No behaviour changes: nothing calls them
until Task 6. Separable because a reviewer can reject the key's composition
while accepting the rendezvous built on it.

**Files:**
- Modify: `shim/undo_shim.c`
- Test: `test/pathpred.c`

**Interfaces:**
- Consumes: `parse_ulong`, the `REAL` macro.
- Produces: `proc_starttime(pid_t)`, `host_parts(char*, char*, char*)`,
  `session_max_age(void)`, `group_key(char *out)`, `armer_is_us(void)`,
  `bucket_ttl(void)`. Task 6 calls all of them.

- [ ] **Step 1: Write the failing C unit tests**

Append to `main()` in `test/pathpred.c`:

```c
    /* FIRST, before anything below calls session_max_age().
     *
     * It caches its answer in a static, and group_key() calls it. Run after
     * group_key this assertion passes on the cached default even if the
     * environment parsing and the floor are both broken -- which is exactly
     * how it was written the first time. A mistuned value must not make every
     * call its own session. */
    {
        setenv("UNDO_SESSION_MAX_AGE", "1", 1);
        expect(session_max_age() >= 60, 1, "max age floored");
        unsetenv("UNDO_SESSION_MAX_AGE");
    }

    /* starttime must be parsed from the last ')' -- field 2 is the executable
     * name in parentheses and may itself contain spaces and parentheses, which
     * is the classic way to misparse this file. */
    expect(proc_starttime(getpid()) > 0, 1, "starttime of ourselves");
    expect(proc_starttime(2147483647) == 0, 1, "starttime of a pid that is gone");

    /* the key is a filename: a hostname is not guaranteed to be one */
    {
        char out[KEY_MAX];
        expect(group_key(out) == 0, 1, "group_key succeeds");
        for (const char *p = out; *p; p++) {
            int ok = (*p >= 'A' && *p <= 'Z') || (*p >= 'a' && *p <= 'z') ||
                     (*p >= '0' && *p <= '9') || *p == '.' || *p == '_' ||
                     *p == '-';
            if (!ok) {
                printf("FAIL: group_key produced %c, not filename-safe\n", *p);
                failures++;
                break;
            }
        }
        char again[KEY_MAX];
        expect(group_key(again) == 0, 1, "group_key succeeds twice");
        expect(strcmp(out, again) == 0, 1, "group_key is stable in one process");
    }

    /* the key must embed the leader's start time, or a recycled pgid merges
     * two unrelated commands into one session */
    {
        char out[KEY_MAX], want[64];
        group_key(out);
        snprintf(want, sizeof want, "-%lu-", proc_starttime(getpgrp()));
        expect(strstr(out, want) != NULL, 1, "key embeds leader starttime");
    }

    /* The roll arithmetic, in isolation. A group's own age cannot be forced
     * from a test, so the bucket is a pure function of age and max age and is
     * tested as one -- otherwise nothing covers the roll at all. */
    expect(age_bucket(0, 21600) == 0, 1, "a fresh group is bucket 0");
    expect(age_bucket(21599, 21600) == 0, 1, "still bucket 0 just before the roll");
    expect(age_bucket(21600, 21600) == 1, 1, "rolls exactly on the boundary");
    expect(age_bucket(86400, 21600) == 4, 1, "a day-old group is bucket 4");
    expect(age_bucket(100, 0) == 0, 1, "a zero max age does not divide by zero");

    /* armer_is_us decides whether this process gets a session at all, and its
     * failure mode is silence, so all its answers are pinned. */
    {
        char arm[64];
        snprintf(arm, sizeof arm, "%d:%lu", (int)getpgrp(),
                 proc_starttime(getpgrp()));
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 1, "UNDO_ARM naming our own group matches");

        snprintf(arm, sizeof arm, "%d:0", (int)getpgrp());
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 0, "UNDO_ARM with a zero start time does not match");

        setenv("UNDO_ARM", "1", 1);
        expect(armer_is_us(), 0, "static UNDO_ARM=1 excludes nothing");

        snprintf(arm, sizeof arm, "%d:%lu", (int)getpgrp() + 12345,
                 proc_starttime(getpgrp()));
        setenv("UNDO_ARM", arm, 1);
        expect(armer_is_us(), 0, "a different pgid does not match");

        unsetenv("UNDO_ARM");
        expect(armer_is_us(), 0, "unset UNDO_ARM excludes nothing");
    }

    /* the three parts must all be present or none: half an identity matches
     * sessions it should not */
    {
        char n[HOST_FIELD], b[HOST_FIELD], p[HOST_FIELD];
        if (host_parts(n, b, p) == 0) {
            expect(*n != 0, 1, "hostname present");
            expect(*b != 0, 1, "boot id present");
            expect(strncmp(p, "pid:[", 5) == 0, 1, "pidns link target shape");
        }
    }
```

Add `#include <sys/types.h>` to `test/pathpred.c` if the build complains about
`pid_t`.

**Keep the max-age block first.** `session_max_age` caches its answer in a
static and `group_key` calls it, so a floor assertion placed after any
`group_key` test asserts against the cached default and passes however broken
the parsing and the floor are. The gate caught exactly that ordering here. If a
later addition calls `session_max_age()` above this block, the test silently
stops testing anything.

- [ ] **Step 2: Run to make sure it fails**

Run: `test/in-container.sh make build/shimunit`
Expected: FAIL to compile — `implicit declaration of function 'proc_starttime'`
and friends.

- [ ] **Step 3: Implement**

In `shim/undo_shim.c`, add `#include <time.h>` to the includes, and add a new
section after the path helpers:

```c
/* ---------- process-group identity ---------- */

#define HOST_FIELD 128
#define KEY_MAX 256

/* Reads the first line of a file into out. Used for /proc entries, which are
 * small and never short-read in practice. */
static int read_first_line(const char *path, char *out, size_t cap)
{
    REAL(open, int, const char *, int, ...);
    int fd = real_open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return -1;
    ssize_t n = read(fd, out, cap - 1);
    close(fd);
    if (n <= 0)
        return -1;
    out[n] = 0;
    char *nl = strchr(out, '\n');
    if (nl)
        *nl = 0;
    return 0;
}

/* Field 22 of /proc/<pid>/stat: the process's start time in clock ticks since
 * boot. It is what makes a pgid unique -- pids are reissued, and a recycled
 * pgid would otherwise merge two unrelated commands into one session, or match
 * a long-dead armer and silently disable capture.
 *
 * Parsed forward from the last ')' rather than by splitting the whole line:
 * field 2 is the executable name in parentheses and may itself contain spaces
 * and parentheses, which is the classic way to misparse this file. */
static unsigned long proc_starttime(pid_t pid)
{
    char path[64], buf[1024];
    snprintf(path, sizeof path, "/proc/%d/stat", (int)pid);
    REAL(open, int, const char *, int, ...);
    int fd = real_open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        return 0;
    ssize_t n = read(fd, buf, sizeof buf - 1);
    close(fd);
    if (n <= 0)
        return 0;
    buf[n] = 0;
    char *p = strrchr(buf, ')');
    if (!p)
        return 0;
    p++;
    /* the token after the name is field 3, so field 22 is 19 tokens later */
    for (int i = 0; i < 20; i++) {
        while (*p == ' ')
            p++;
        if (!*p)
            return 0;
        if (i == 19)
            break;
        while (*p && *p != ' ')
            p++;
    }
    return parse_ulong(p);
}

/* The three parts of a host identity, matching internal/session.composeHost
 * byte for byte: hostname, boot id, and the raw /proc/self/ns/pid link target.
 *
 * All three or failure. A session whose host file disagrees with what the Go
 * binary computes reads as foreign on the very machine that created it, is
 * pinned for the whole grace, and undo refuses to revert it. */
static int host_parts(char *name, char *boot, char *pidns)
{
    if (gethostname(name, HOST_FIELD) != 0)
        return -1;
    name[HOST_FIELD - 1] = 0;
    if (!*name)
        return -1;
    if (read_first_line("/proc/sys/kernel/random/boot_id", boot, HOST_FIELD) != 0)
        return -1;
    if (!*boot)
        return -1;
    ssize_t n = readlink("/proc/self/ns/pid", pidns, HOST_FIELD - 1);
    if (n <= 0)
        return -1;
    pidns[n] = 0;
    return 0;
}

/* UNDO_SESSION_MAX_AGE in seconds: how long one process group's session lasts
 * before it rolls to a new one.
 *
 * Only long-lived groups reach it; an ordinary command lasting seconds never
 * splits, because the bucket is measured from the group's own start rather
 * than from wall-clock boundaries. Floored so a mistuned value cannot make
 * every call its own session. */
static unsigned long session_max_age(void)
{
    static unsigned long v;
    if (!v) {
        v = parse_ulong(getenv("UNDO_SESSION_MAX_AGE"));
        if (!v)
            v = 21600; /* six hours */
        else if (v < 60)
            v = 60;
    }
    return v;
}

/* Seconds this process group has been alive, from /proc/uptime and the
 * leader's start time.
 *
 * Uptime rather than wall clock deliberately: starttime is in ticks since
 * boot, and converting it to wall clock needs boot-time arithmetic that
 * misbehaves across container boundaries and NTP steps. Both values here share
 * the same origin, so the subtraction is exact without knowing when boot was. */
static unsigned long group_age(unsigned long starttime_ticks)
{
    char buf[64];
    if (read_first_line("/proc/uptime", buf, sizeof buf) != 0)
        return 0;
    unsigned long up = parse_ulong(buf); /* integer seconds is plenty */
    long hz = sysconf(_SC_CLK_TCK);
    if (hz <= 0)
        hz = 100;
    unsigned long started = starttime_ticks / (unsigned long)hz;
    return up > started ? up - started : 0;
}

/* Which session slice a group of this age belongs to.
 *
 * Its own function because a test cannot make a process group old, so this is
 * the only part of the roll that can be asserted directly. Guards max == 0
 * because session_max_age never returns it but a future caller might. */
static unsigned long age_bucket(unsigned long age, unsigned long max)
{
    if (max == 0)
        return 0;
    return age / max;
}

/* Appends s to a key being built, replacing anything that is not
 * filename-safe. The key is a directory entry name and a hostname is not
 * guaranteed to be one. */
static void key_append(char *dst, size_t cap, size_t *len, const char *s,
                       size_t max)
{
    for (size_t i = 0; s[i] && i < max; i++) {
        char c = s[i];
        int ok = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                 (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-';
        if (*len + 1 >= cap)
            return;
        dst[(*len)++] = ok ? c : '_';
    }
}

/* The key identifying this process group's current session.
 *
 *   <hostname>-<boot-id>-<pidns>-<pgid>-<leader-starttime>-<age-bucket>
 *
 * Origin is in the key because homes are shared across nodes and two machines
 * will both have pgid 5000; the boot id because a reboot reissues pgids; the
 * pid namespace because containers sharing a kernel and a UTS namespace have
 * separate ones, where the same number names unrelated processes. The leader's
 * starttime defeats pgid reuse within one boot. The bucket bounds a long-lived
 * group; see session_max_age.
 *
 * An unreadable starttime records 0 rather than failing: two commands that
 * reuse a pgid can then merge into one session, which is visible in undo show
 * and never a wrong restore, whereas failing would mean no capture at all. */
static int group_key(char *out)
{
    char name[HOST_FIELD], boot[HOST_FIELD], pidns[HOST_FIELD];
    if (host_parts(name, boot, pidns) != 0)
        return -1;
    pid_t pgid = getpgrp();
    unsigned long st = proc_starttime(pgid);
    unsigned long age = group_age(st);
    unsigned long bucket = age_bucket(age, session_max_age());

    size_t len = 0;
    key_append(out, KEY_MAX, &len, name, 64);
    key_append(out, KEY_MAX, &len, "-", 1);
    key_append(out, KEY_MAX, &len, boot, 40);
    key_append(out, KEY_MAX, &len, "-", 1);
    key_append(out, KEY_MAX, &len, pidns, 32);
    char tail[64];
    snprintf(tail, sizeof tail, "-%d-%lu-%lu", (int)pgid, st, bucket);
    key_append(out, KEY_MAX, &len, tail, sizeof tail);
    if (len >= KEY_MAX)
        return -1;
    out[len] = 0;
    return 0;
}

/* When this process group's current bucket ends, in wall clock, for a reader
 * on any node to compare against. */
static time_t bucket_ttl(void)
{
    unsigned long max = session_max_age();
    unsigned long age = group_age(proc_starttime(getpgrp()));
    return time(NULL) + (time_t)(max - (age % max));
}

/* True when this process is in the process group that armed us, which
 * therefore gets no session of its own: an agent harness or a login shell
 * should not journal its own housekeeping.
 *
 * Compares the starttime as well as the pgid. A bare pgid comparison would let
 * a recycled number match a long-dead armer -- inherited through a terminal
 * multiplexer, say -- and the symptom of that is silence.
 *
 * A zero start time is rejected rather than compared. proc_starttime answers 0
 * when it cannot read, so comparing against a recorded 0 would let "could not
 * read" masquerade as "matches", and matching means this process creates no
 * session at all. Erring toward not-the-armer costs at most an extra session;
 * erring the other way loses capture silently. A real start time is ticks
 * since boot and is never 0 in practice -- measured at 27915037 for pid 1 on a
 * host up three days. */
static int armer_is_us(void)
{
    const char *arm = getenv("UNDO_ARM");
    if (!arm || !*arm)
        return 0;
    const char *colon = strchr(arm, ':');
    if (!colon)
        return 0; /* the static UNDO_ARM=1 form: no identity, no exclusion */
    unsigned long apgid = parse_ulong(arm);
    unsigned long ast = parse_ulong(colon + 1);
    if (apgid == 0 || ast == 0)
        return 0;
    pid_t pgid = getpgrp();
    return (unsigned long)pgid == apgid && proc_starttime(pgid) == ast;
}
```

- [ ] **Step 4: Run the unit tests**

Run: `test/in-container.sh bash -c 'make build/shimunit && ./build/shimunit'`

Both halves must be inside **one** `in-container.sh` invocation: each call
starts a fresh container against a fresh copy of the tree, so a binary built by
one call does not exist for the next.
Expected: `path predicates ok`, no FAIL lines, exit 0.

Run: `test/in-container.sh make test`
Expected: PASS. Nothing calls the new functions yet, so no behaviour moved.

Run the floor check.
Expected: `GLIBC_2.34`. `gethostname`, `sysconf` and `time` are all
`GLIBC_2.2.5`.

- [ ] **Step 5: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add shim/undo_shim.c test/pathpred.c
git diff --cached --stat
git commit -m "shim: identify the process group a session belongs to"
```

---

### Task 6: Lazy session creation

**Files:**
- Modify: `shim/undo_shim.c` (`armed`, plus a new session-resolution section)
- Modify: `test/e2e.sh` (cases 41–45)

**Interfaces:**
- Consumes: `group_key`, `bucket_ttl`, `armer_is_us`, `host_parts`,
  `proc_starttime`, `session_dir` (Task 4's version).
- Produces: sessions carrying `pgid` and `ttl`, which Task 3's `Live` reads.

- [ ] **Step 1: Write the failing e2e cases**

Append to `test/e2e.sh`:

```bash
# arms a command the way `undo arm` will: environment only, no session
run_agent() {
    env UNDO_DATA_DIR="$UNDO_DATA_DIR" \
        UNDO_ARM="$(arm_id)" \
        LD_PRELOAD="$LIB" setsid bash -c "$*"
    sleep 0.01
}

echo "== case 41: an agent tool call is captured with no shell hook at all"
make_tree
before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
run_agent "rm -rf $PLAY/docs"
[[ ! -e $PLAY/docs ]] || fail "case 41: the rm did not run"
after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
[[ $after -gt $before ]] || fail "case 41: no session was created"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
[[ -s $sess/journal ]] || fail "case 41: nothing was journaled"
[[ -s $sess/pgid ]] || fail "case 41: no pgid recorded"
[[ -s $sess/ttl ]] || fail "case 41: no ttl recorded"
grep -q 'rm -rf' "$sess/cmd" || fail "case 41: cmd did not capture the command line"
"$UNDO" apply "$last" -y >/dev/null
[[ -f $PLAY/docs/report.txt ]] || fail "case 41: the tree did not come back"

echo "== case 42: a read-only tool call creates no session at all"
before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
run_agent "cat $PLAY/top.txt >/dev/null && ls $PLAY >/dev/null"
after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
[[ $after -eq $before ]] ||
    fail "case 42: creation is lazy; a command that destroyed nothing made a session"

echo "== case 43: the arming process group gets no session of its own"
make_tree
before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
# UNDO_ARM naming *this* process group, and the command runs in it
env UNDO_DATA_DIR="$UNDO_DATA_DIR" \
    UNDO_ARM="$(arm_id)" \
    LD_PRELOAD="$LIB" bash -c "rm $PLAY/top.txt"
[[ ! -e $PLAY/top.txt ]] || fail "case 43: the rm did not run"
after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
[[ $after -eq $before ]] ||
    fail "case 43: the armer's own group was journaled; an agent harness would \
record its own housekeeping"

echo "== case 44: concurrent members of one group agree on one complete session"
# Repeated, because both failures this covers are races: a peer following the
# rendezvous link into a session whose data/ does not exist yet, and two first
# writers each concluding the other declared the integrity contract. One
# iteration would pass by luck.
for round in 1 2 3 4 5; do
    make_tree
    before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
    run_agent "rm $PLAY/docs/report.txt & rm $PLAY/docs/sub/note.txt & \
               rm $PLAY/top.txt & wait"
    after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
    [[ $((after - before)) -eq 1 ]] ||
        fail "case 44 round $round: $((after - before)) sessions for one process group, want 1"
    last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
    sess=$UNDO_DATA_DIR/sessions/$last
    [[ -d $sess/data ]] || fail "case 44 round $round: the session has no data/"
    [[ -f $sess/journalv ]] ||
        fail "case 44 round $round: no journalv; both first writers decided the \
other had declared the integrity contract, and this session is unchecked forever"
    [[ $(grep -c '^unlink' "$sess/journal") -eq 3 ]] ||
        fail "case 44 round $round: all three deletions should be in the one session"
    # nothing may have been lost to a half-built session
    ! grep -q '^lost' "$sess/journal" ||
        fail "case 44 round $round: a peer saved into a session that was not \
finished being built, and the file it deleted is unprotected"
    "$UNDO" apply "$last" -y >/dev/null
done

echo "== case 45: the shim never breaks the command, even with no writable store"
make_tree
env UNDO_DATA_DIR=/proc/nonexistent/store \
    UNDO_ARM="1" LD_PRELOAD="$LIB" setsid bash -c "rm $PLAY/top.txt"
rc=$?
[[ $rc -eq 0 ]] || fail "case 45: an unwritable store changed the command's exit status"
[[ ! -e $PLAY/top.txt ]] || fail "case 45: the rm did not run"
```

- [ ] **Step 2: Run to make sure they fail**

Run: `test/in-container.sh bash test/e2e.sh`
Expected: FAIL at case 41 — no session was created. Cases 41, 42 and 44 pass
vacuously today (nothing is ever created, and the shim already fails open);
they are there to stay true, and case 44 fails with 0 sessions.

- [ ] **Step 3: Implement session resolution**

In `shim/undo_shim.c`, add a section after the group-identity section:

```c
/* ---------- lazy session creation ---------- */

/* The store root, from the environment. The shim never computes the XDG
 * default: whoever armed us knows it, and guessing would put backups somewhere
 * the CLI does not look. */
static const char *data_dir(void)
{
    const char *d = getenv("UNDO_DATA_DIR");
    if (!d || !*d || *d != '/')
        return NULL;
    return d;
}

/* Writes body to path, creating it. Reports whether it landed: some of what
 * fill_session writes decides liveness, and best-effort is the wrong policy
 * for those. */
static int write_meta(const char *dir, const char *name, const char *body)
{
    char path[PATH_MAX];
    if ((size_t)snprintf(path, sizeof path, "%s/%s", dir, name) >= sizeof path)
        return -1;
    REAL(open, int, const char *, int, ...);
    int fd = real_open(path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
    if (fd < 0)
        return -1;
    size_t len = strlen(body);
    ssize_t n = write(fd, body, len);
    /* close() is where a network filesystem reports a deferred write error:
     * write() can return the full length into the page cache and the ENOSPC,
     * EDQUOT or EIO only appears here. The store sits on a quota'd network
     * home, so this is the expected failure on the deployment target, and
     * ignoring it would publish a session whose liveness metadata never
     * reached the server. */
    int crc = close(fd);
    return (n >= 0 && (size_t)n == len && crc == 0) ? 0 : -1;
}

/* The command line of the group leader, which for an agent tool call is
 * `<shell> -c '<the whole command>'` -- so the pipeline survives intact,
 * because the entire command is one argv element.
 *
 * Falls back to our own cmdline when the leader has already exited. */
static void group_cmdline(pid_t pgid, char *out, size_t cap)
{
    char path[64];
    snprintf(path, sizeof path, "/proc/%d/cmdline", (int)pgid);
    REAL(open, int, const char *, int, ...);
    int fd = real_open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0)
        fd = real_open("/proc/self/cmdline", O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        snprintf(out, cap, "(unknown)\n");
        return;
    }
    ssize_t n = read(fd, out, cap - 2);
    close(fd);
    if (n <= 0) {
        snprintf(out, cap, "(unknown)\n");
        return;
    }
    for (ssize_t i = 0; i < n; i++)
        if (out[i] == 0)
            out[i] = ' ';
    while (n > 0 && out[n - 1] == ' ')
        n--;
    out[n] = '\n';
    out[n + 1] = 0;
}

/* Fills the metadata of a session directory. Every value describes the group
 * rather than the writer, so two members racing here write byte-identical
 * content and the race is invisible. */
static void fill_session(const char *dir, pid_t pgid)
{
    char buf[PATH_MAX];

    group_cmdline(pgid, buf, sizeof buf);
    write_meta(dir, "cmd", buf);

    /* pid and pgid are the SAME number here, and must stay that way.
     * Both name the group leader, and Session.probe depends on it: when
     * pgid is 1 it cannot use kill(-1, 0) and falls back to the pid,
     * which is safe only because pgid 1 implies pid 1 -- init, which
     * cannot exit while its children are still running. Recording the
     * creating process's pid instead would put a dead leader pid beside
     * an unprobeable group and make a running command collectible. A
     * review gate raised that scenario; this is what keeps it
     * hypothetical. */
    snprintf(buf, sizeof buf, "%d\n", (int)pgid);
    write_meta(dir, "pid", buf); /* an older undo probes this */
    write_meta(dir, "pgid", buf);

    snprintf(buf, sizeof buf, "%lld\n", (long long)bucket_ttl());
    write_meta(dir, "ttl", buf);

    char name[HOST_FIELD], boot[HOST_FIELD], pidns[HOST_FIELD];
    if (host_parts(name, boot, pidns) == 0) {
        snprintf(buf, sizeof buf, "%s\t%s\t%s\n", name, boot, pidns);
        write_meta(dir, "host", buf);
    }
}

/* Removes a session directory this thread built and then did not use, because
 * another member of the group published its own first.
 *
 * By name, never by walking the directory. This runs inside an interposer, and
 * a recursive delete driven by directory contents is precisely the thing that
 * must never appear here by accident. Anything left behind is an empty session
 * with no journal: it costs no retention slot and gc collects it. */
static void discard_session(const char *dir)
{
    static const char *const names[] = {"cmd",  "pid",      "pgid",
                                        "ttl",  "host",     "journalv", NULL};
    char path[PATH_MAX];
    REAL(unlink, int, const char *);
    REAL(rmdir, int, const char *);
    for (int i = 0; names[i]; i++)
        if ((size_t)snprintf(path, sizeof path, "%s/%s", dir, names[i]) <
            sizeof path)
            real_unlink(path);
    if ((size_t)snprintf(path, sizeof path, "%s/data", dir) < sizeof path)
        real_rmdir(path);
    real_rmdir(dir);
}

/* Resolves this process group's session directory into `out`, creating it if
 * this is the group's first destructive call.
 *
 * The ordering is not the obvious one. mkdir of the session directory comes
 * first and is the arbiter for id collisions between *unrelated* groups: two
 * groups creating in the same microsecond would otherwise agree on one id and
 * interleave their journals. The symlink then decides which member of *this*
 * group won. */
static int resolve_session(char *out)
{
    const char *data = data_dir();
    if (!data)
        return -1;
    char key[KEY_MAX];
    if (group_key(key) != 0)
        return -1;

    char groups[PATH_MAX], link[PATH_MAX];
    if ((size_t)snprintf(groups, sizeof groups, "%s/groups", data) >= sizeof groups)
        return -1;
    if ((size_t)snprintf(link, sizeof link, "%s/%s", groups, key) >= sizeof link)
        return -1;

    REAL(mkdir, int, const char *, mode_t);
    char sessions[PATH_MAX];
    if ((size_t)snprintf(sessions, sizeof sessions, "%s/sessions", data) >=
        sizeof sessions)
        return -1;
    real_mkdir(data, 0700);
    real_mkdir(sessions, 0700);
    real_mkdir(groups, 0700);
    /* EEXIST is the ordinary case, so those returns say nothing; what matters
     * is what is actually there. These names are predictable, and on shared
     * storage another user can pre-create one as a symlink -- after which
     * sessions, metadata and rendezvous links all land wherever it points, and
     * discard_session removes files there. ensure_store guards the
     * per-filesystem store for exactly this reason; the same reasoning applies
     * here and was missed. Raised by the gate, pinned by e2e case 46. */
    if (!own_real_dir(data) || !own_real_dir(sessions) || !own_real_dir(groups))
        return -1;

    for (int attempt = 0; attempt < 2; attempt++) {
        char target[PATH_MAX];
        ssize_t n = readlink(link, target, sizeof target - 1);
        if (n > 0) {
            target[n] = 0;
            const char *slash = strrchr(target, '/');
            const char *id = slash ? slash + 1 : target;
            if ((size_t)snprintf(out, PATH_MAX, "%s/%s", sessions, id) >= PATH_MAX)
                return -1;
            if (own_real_dir(out))
                return 0;
            /* The session it names is gone -- reachable through
             * `undo purge --force` on a live session. Without this the journal
             * open lands in a directory that does not exist and every later
             * record is lost with nothing printed. */
            REAL(unlink, int, const char *);
            real_unlink(link);
            continue;
        }

        /* pick an id the same way session.Create does, so sessionID() accepts
         * it and the orphan sweep can reclaim its store */
        struct timespec ts;
        char id[32], dir[PATH_MAX];
        int made = 0;
        for (int tries = 0; tries < 1000 && !made; tries++) {
            if (clock_gettime(CLOCK_REALTIME, &ts) != 0)
                return -1;
            snprintf(id, sizeof id, "%lld%06ld", (long long)ts.tv_sec,
                     (long)(ts.tv_nsec / 1000));
            if ((size_t)snprintf(dir, sizeof dir, "%s/%s", sessions, id) >= sizeof dir)
                return -1;
            if (real_mkdir(dir, 0700) == 0)
                made = 1;
            else if (errno != EEXIST)
                return -1;
        }
        if (!made)
            return -1;

        /* Everything the session needs, BEFORE the link that publishes it.
         *
         * A peer follows the link the instant it appears and starts saving
         * into the directory it names. If data/ does not exist yet its backup
         * fails while the real deletion still succeeds, so the file goes
         * unprotected -- and the metadata a reader needs to judge liveness is
         * missing too. Publishing a half-built session is the race; building
         * it first is the fix. */
        char data_sub[PATH_MAX];
        if ((size_t)snprintf(data_sub, sizeof data_sub, "%s/data", dir) >=
            sizeof data_sub) {
            discard_session(dir);
            return -1;
        }
        real_mkdir(data_sub, 0700);
        /* The mkdir's own return says nothing useful -- EEXIST is the ordinary
         * case when a peer got here first -- so what is checked is that a
         * directory we own is there now. Publishing without it would hand
         * every later call in this process group a session whose backups
         * cannot be written, while their deletions succeed exactly as asked.
         * One broken session would quietly unprotect the rest of the command. */
        if (!own_real_dir(data_sub)) {
            discard_session(dir);
            return -1;
        }
        /* A session published without pid, pgid and ttl loads with Pgid 0 and
         * Pid 0, probe() answers false, and gc removes the backups of a
         * command that is still running. Leaving the operation uncaptured is
         * recoverable; that is not. */
        if (fill_session(dir, getpgrp()) != 0) {
            discard_session(dir);
            return -1;
        }

        /* ../sessions/<id>, not the bare id: a symlink target resolves
         * relative to the link's own parent, so a bare id would resolve to
         * groups/<id> -- a path that never exists. Every group link would be
         * dangling, and the gc prune would delete the mapping of every live
         * session on its first run. */
        char rel[PATH_MAX];
        if ((size_t)snprintf(rel, sizeof rel, "../sessions/%s", id) >= sizeof rel)
            return -1;
        if (symlink(rel, link) == 0) {
            snprintf(out, PATH_MAX, "%s", dir);
            return 0;
        }
        /* another member of our group published first; take theirs */
        discard_session(dir);
    }
    return -1;
}

/* This process's session directory, cached.
 *
 * Thread-local like the journal descriptor and the store cache above: a
 * process-wide table would need a lock, and taking a lock inside an interposer
 * invites deadlock and leaves undefined state across fork().
 *
 * The key is re-derived on every call rather than cached, because it carries
 * the age bucket: when the bucket rolls the key changes, and re-resolving is
 * exactly what moves this process to the new session. The previous one is
 * already retired by its own ttl and needs nothing done to it. */
static __thread char lazy_dir[PATH_MAX];
static __thread char lazy_key[KEY_MAX];

static const char *lazy_session(void)
{
    /* An empty UNDO_ARM is unset, not armed. `env UNDO_ARM= ...` yields a
     * non-NULL empty string, so a bare NULL test would arm on it -- and it
     * would arm with no identity at all, which is the one state where neither
     * the exclusion nor the detach test can run. Every other reader of this
     * variable already treats empty as absent; this one must agree. */
    const char *arm = getenv("UNDO_ARM");
    if (!arm || !*arm)
        return NULL;
    if (armer_is_us())
        return NULL;
    char key[KEY_MAX];
    if (group_key(key) != 0)
        return NULL;
    if (lazy_dir[0] && strcmp(lazy_key, key) == 0)
        return lazy_dir;
    char dir[PATH_MAX];
    if (resolve_session(dir) != 0)
        return NULL;
    snprintf(lazy_dir, sizeof lazy_dir, "%s", dir);
    snprintf(lazy_key, sizeof lazy_key, "%s", key);
    return lazy_dir;
}
```

- [ ] **Step 4: Route the shim through it**

`session_dir` is called from `journal_fd`, `session_id`, `backup_name`,
`evac_name`, `other_journal_fd` and `mod_seen`, so making it answer with the
lazy session covers every one of them at once.

In `shim/undo_shim.c`, rename Task 4's `session_dir` to `explicit_session` and
add below it:

```c
/* The session this process writes to: an explicit UNDO_SESSION when one was
 * set for us, otherwise one created lazily for our process group.
 *
 * Explicit wins, so the existing shell hooks are untouched and the two
 * mechanisms never fight. */
static const char *session_dir(void)
{
    const char *s = explicit_session();
    if (s)
        return s;
    return lazy_session();
}
```

`lazy_session` is defined after `session_dir` in file order, so add a forward
declaration above `session_dir`:

```c
static const char *lazy_session(void);
```

**`armed()` DOES need changing, and the opposite of what this plan first said.**
It used to read `!in_shim && session_dir() != NULL`, and `session_dir()` now
creates. Since `armed()` runs on every interposed call including read-only
opens, `cat` and `ls` would each mint a session directory, six metadata files
and a group symlink while recording nothing -- destroying the "read-only tool
calls cost nothing" property the design rests its metadata-traffic answer on.
Caught by e2e case 42.

Creation belongs where something is about to be written. `session_dir()` is
already called only from those paths, so the fix is to take the test out of
`armed()`:

```c
static int armed(void)
{
    if (in_shim)
        return 0;
    if (explicit_session() != NULL)
        return 1;
    const char *arm = getenv("UNDO_ARM");
    if (!arm || !*arm)
        return 0;
    /* armer_is_us() reads /proc, too expensive on every interposed call, so
     * its answer is cached against the process group it describes -- the only
     * thing it depends on, and it changes only through setpgid/setsid. */
    static __thread pid_t cached_pgid;
    static __thread int cached_excluded;
    pid_t pg = getpgrp();
    if (pg != cached_pgid) {
        cached_pgid = pg;
        cached_excluded = armer_is_us();
    }
    return !cached_excluded;
}
```

`armed()` must then move below `armer_is_us`, and `own_real_dir` must move up
to the path helpers -- `resolve_session` calls it and it lived in the
store-placement section below. Both are the same cross-task ordering class as
`parse_ulong` in Task 4: four instances now, every one found by the compiler
rather than by review, because the plan is reviewed per task and the file
compiles as a whole.

**Order matters in the file.** `explicit_session` and `session_dir` sit near
the top, above `journal_fd`; `lazy_session` depends on `group_key`, which
depends on `parse_ulong`.

**`parse_ulong` has already been moved** — Task 4 needed it for `session_dir`'s
`UNDO_SID` comparison, so it now sits just below the `REAL` macro with a marker
left where it used to be. Nothing to do here; place the group-identity section
above `session_dir` and it will resolve.

That move was originally scheduled for this task, and Task 4 hit an
implicit-declaration error because of it. Worth noting why no review caught it:
the plan is reviewed one task at a time, but the file compiles as a whole, so a
dependency introduced before the plan satisfies it is invisible per task. If a
later task introduces another such ordering dependency, expect the compiler to
be what finds it.

**Case numbering.** Task 6 gained case 46 (the symlinked sessions directory)
during execution, so this task's cases are 47-51, not 46-49 as first written.

**Case 49 must not identify a session by `ls | tail -1`.** When nothing is
captured `undo run` removes its own empty session, so the newest session is some
earlier case's -- and asserting against it can fail on working code and pass on
broken code. It counts sessions instead. Worth knowing: that bug was invisible
when the case ran in isolation, because then there was no earlier session for
`tail -1` to find. A case whose correctness depends on suite ordering is one
someone will "fix" by running it alone, seeing it pass, and calling the suite
flaky.

- [ ] **Step 5: Run everything**

Run: `test/in-container.sh make test`
Expected: PASS, including cases 41–45. Every earlier case sets `UNDO_SESSION`
explicitly and never sets `UNDO_ARM`, so they take the unchanged path.

Run: `test/in-container.sh --privileged test/multifs.sh`
Expected: PASS. Store placement is unchanged; only where the session lives
moved.

Run the floor check.
Expected: `GLIBC_2.34`. `clock_gettime` moved into libc at 2.17 and `symlink`
is 2.2.5.

- [ ] **Step 6: Prove case 43 discriminates**

Temporarily make `lazy_session` skip the `armer_is_us()` check and rerun the
e2e suite. Confirm case 43 FAILS. Restore.

- [ ] **Step 7: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add shim/undo_shim.c test/e2e.sh
git diff --cached --stat
git commit -m "shim: create a session for the process group on its first destructive call"
```

---

### Task 7: `undo arm` and the doctor arming section

**Files:**
- Modify: `cmd/undo/main.go` (usage, pre-parse dispatch, `cmdArm`)
- Modify: `cmd/undo/doctor.go` (the arming section — `cmdDoctor` lives here,
  not in `main.go`)
- Modify: `test/e2e.sh` (cases 46–49)

**Interfaces:**
- Consumes: the `UNDO_ARM`, `UNDO_SID` and `UNDO_DATA_DIR` contracts from
  Tasks 4–6.
- Produces: `undo arm -- <harness> [args...]`. Reuses the existing
  `findShim()` and `isUndoShim()` from `cmd/undo/run.go`, and does **not**
  replace `undo run`, which already covers the single-command case.

- [ ] **Step 1: Write the failing e2e case**

Append to `test/e2e.sh`:

```bash
echo "== case 46: undo arm captures what the armed program spawns"
# The harness shape: undo arm execs a long-lived program, and the tool calls it
# starts each get their own process group. setsid stands in for that here --
# it is what the program spawns, not the program itself, that is captured.
make_tree
before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
UNDO_LIB=$LIB "$UNDO" arm -- bash -c "setsid rm $PLAY/top.txt; sleep 0.2"
[[ ! -e $PLAY/top.txt ]] || fail "case 46: the rm did not run"
after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
[[ $after -gt $before ]] || fail "case 46: undo arm captured nothing"
"$UNDO" -y >/dev/null
[[ -f $PLAY/top.txt ]] || fail "case 46: the file did not come back"

echo "== case 47: undo arm does NOT capture the armed program itself"
# The armed program is the armer and is excluded by design. Asserting it keeps
# anyone from "fixing" undo arm into a single-command runner, which would make
# an agent journal its own housekeeping. undo run is the single-command path.
make_tree
before=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
UNDO_LIB=$LIB "$UNDO" arm -- rm "$PLAY/top.txt"
[[ ! -e $PLAY/top.txt ]] || fail "case 47: the rm did not run"
after=$(ls "$UNDO_DATA_DIR/sessions" | wc -l)
[[ $after -eq $before ]] ||
    fail "case 47: undo arm captured the armed program itself; it is the armer \
and must be excluded, or an agent journals its own housekeeping"

echo "== case 48: undo run still captures a single command, and sets UNDO_SID"
make_tree
UNDO_LIB=$LIB "$UNDO" run -- rm "$PLAY/top.txt"
[[ ! -e $PLAY/top.txt ]] || fail "case 48: the rm did not run"
"$UNDO" -y >/dev/null
[[ -f $PLAY/top.txt ]] || fail "case 48: undo run stopped capturing"
# a daemon the command starts must not inherit the session
make_tree
UNDO_LIB=$LIB "$UNDO" run -- bash -c "setsid rm $PLAY/top.txt; sleep 0.2"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
! grep -q "$PLAY/top.txt" "$UNDO_DATA_DIR/sessions/$last/journal" 2>/dev/null ||
    fail "case 48: a detached child wrote into undo run's session, which is \
marked done the moment the command exits"

echo "== case 49: doctor reports that capture is disabled in the armer's own group"
out=$(UNDO_LIB=$LIB "$UNDO" arm -- "$UNDO" doctor 2>&1 || true)
grep -qi 'arm' <<<"$out" || fail "case 49: doctor says nothing about arming"
```

- [ ] **Step 2: Run to make sure it fails**

Run: `test/in-container.sh bash test/e2e.sh`
Expected: FAIL at case 46 — `undo: unknown command "arm"` (the usage text is
printed and the exit status is 2). Cases 48's first half should PASS already:
`undo run` exists and this pins that it keeps working.

- [ ] **Step 3: Implement `undo arm`**

In `cmd/undo/main.go`, add to the usage text after the `doctor` line:

```
  undo arm -- <harness>   arm an agent so everything it runs is captured
```

**`undo arm` is dispatched BEFORE flag parsing, not from the command switch.**
undo's parser consumes `-n`, `-y`, `--force`, `--purge`, `-i`, `-V`, `-h` and
`help` anywhere in argv, so from the switch `undo arm -- claude -y` swallows
`-y` as undo's own flag and `undo arm -- foo --help` prints undo's usage instead
of running anything. `undo run` already avoids this the same way; the design
notes that and this plan then put `arm` in the switch regardless. Case 51 pins
it.

In `main()`, directly below the existing `undo run` special case:

```go
	// `undo arm` likewise takes the rest of argv verbatim: the armed
	// program's own flags must not be read as undo's.
	if len(os.Args) > 1 && os.Args[1] == "arm" {
		cmdArm(os.Args[2:])
		return
	}
```

Note it passes `os.Args[2:]`, not the parsed `args`: from the switch, `args[0]`
is the literal string "arm" and cmdArm would try to exec it.

Add the implementation:

```go
**Do not write a shim-locating function.** `findShim()` already exists in
`cmd/undo/run.go` and handles the env override, the dev tree, the user install
and the system paths. Call it. Likewise `isUndoShim()` is the existing test for
"is this LD_PRELOAD entry one of ours", and `undo run` already exists in full —
this task adds `undo arm` beside it, not in place of it.

`procStatField` and `selfStatField` were added to `cmd/undo/run.go` in Task 4.
Use them; do not define them again.

```go

// cmdArm execs a harness with the shim preloaded, so that everything the
// harness spawns is captured.
//
// The harness itself is the armer and is therefore excluded: it should not
// journal its own housekeeping, while each tool call it starts gets a fresh
// process group and a session of its own.
//
// This is deliberately NOT a way to run one command. exec preserves the
// process group, so a single command run this way would land in the very group
// UNDO_ARM names and record nothing at all, with no error and no output. Use
// the existing `undo run -- <cmd>` for that; it creates the session eagerly and
// knows when the command ended, which this cannot.
//
// exec rather than fork: it preserves pid, pgid and sid, so the identity
// recorded in UNDO_ARM and UNDO_SID is exactly the harness's. Forking would
// record this process's, which dies immediately, and the exclusion test would
// then match nothing.
func cmdArm(args []string) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fatal(fmt.Errorf("arm needs a program: undo arm -- <harness> [args...]\n" +
			"to capture a single command, use: undo run -- <cmd> [args...]"))
	}
	lib := findShim() // cmd/undo/run.go
	if lib == "" {
		fatal(fmt.Errorf("libundo.so not found; set UNDO_LIB or make install"))
	}
	prog, err := exec.LookPath(args[0])
	if err != nil {
		fatal(err)
	}

	env := os.Environ()
	set := func(k, v string) {
		pfx := k + "="
		for i, e := range env {
			if strings.HasPrefix(e, pfx) {
				env[i] = pfx + v
				return
			}
		}
		env = append(env, pfx+v)
	}
	// Drop any other libundo.so first: two loaded copies both interpose,
	// duplicating journal entries and recording each other's backups.
	var keep []string
	for _, p := range strings.Split(os.Getenv("LD_PRELOAD"), ":") {
		if p != "" && !isUndoShim(p) { // isUndoShim: cmd/undo/run.go
			keep = append(keep, p)
		}
	}
	set("LD_PRELOAD", strings.Join(append([]string{lib}, keep...), ":"))
	set("UNDO_DATA_DIR", filepath.Dir(session.Root()))
	set("UNDO_HOOK", "arm")
	// pgid is field 5 of our own stat, but the start time must come from the
	// LEADER's stat, not ours.
	//
	// When undo arm is launched without job control it is not the group leader,
	// and pairing the leader's pgid with our own start time describes no
	// process at all. The shim's armer_is_us compares that pair against
	// proc_starttime(pgid), so it would never match, and the armed harness's
	// own housekeeping would be captured instead of excluded -- silently, since
	// the only symptom is extra sessions nobody asked for.
	pgid := selfStatField(5)
	st := ""
	if pgid != "" {
		st = procStatField("/proc/"+pgid+"/stat", 22)
	}
	if pgid != "" && st != "" {
		set("UNDO_ARM", pgid+":"+st)
	} else {
		// No identity: lazy creation still works, but the exclusion and detach
		// tests cannot run. doctor says so rather than letting it pass.
		set("UNDO_ARM", "1")
	}
	if sid := selfStatField(6); sid != "" {
		set("UNDO_SID", sid)
	}

	if err := syscall.Exec(prog, args, env); err != nil {
		fatal(err)
	}
}
```

Add `"os/exec"` and `"syscall"` to the imports of `cmd/undo/main.go` if not
already present.

- [ ] **Step 4: Add the doctor arming section**

The silent failure mode of this whole design is *every process shares the
armer's group, so nothing is ever captured* — a container with no job control
produces exactly that. Doctor is the only thing that can say so.

In `cmdDoctor`, append:

```go
	fmt.Println()
	fmt.Println("arming:")
	arm := os.Getenv("UNDO_ARM")
	switch {
	case arm == "":
		fmt.Println("  UNDO_ARM       not set (env arming off; hooks may still be active)")
	case !strings.Contains(arm, ":"):
		fmt.Println("  UNDO_ARM       set, but with no identity (UNDO_ARM=1)")
		fmt.Println("                 the armer-exclusion and detach tests are DISABLED")
	default:
		fmt.Println("  UNDO_ARM      ", arm)
	}
	if p := os.Getenv("LD_PRELOAD"); strings.Contains(p, "libundo.so") {
		fmt.Println("  LD_PRELOAD     shim loaded")
	} else {
		fmt.Println("  LD_PRELOAD     shim NOT loaded; nothing in this process is captured")
	}
	if sid := os.Getenv("UNDO_SID"); sid != "" {
		fmt.Println("  UNDO_SID      ", sid, "(detach test active)")
	} else {
		fmt.Println("  UNDO_SID       not set; an inherited UNDO_SESSION is trusted unconditionally")
	}
	if pgid := selfStatField(5); pgid != "" && strings.HasPrefix(arm, pgid+":") {
		fmt.Println("  process group  same as the armer's: capture is DISABLED here.")
		fmt.Println("                 expected for the agent process itself; if every")
		fmt.Println("                 command reports this, nothing creates process groups")
		fmt.Println("                 (a container with no job control) and nothing is captured.")
	}
	ign := os.Getenv("UNDO_IGNORE")
	if ign == "" {
		ign = "(none beyond the built-in defaults)"
	}
	fmt.Println("  UNDO_IGNORE   ", ign)
```

- [ ] **Step 5: Run everything**

Run: `test/in-container.sh make test`
Expected: PASS, including cases 46 through 49.

- [ ] **Step 6: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add cmd/undo/main.go test/e2e.sh
git diff --cached --stat
git commit -m "undo arm: capture an agent's commands without a shell hook"
```

---

### Task 8: Prune group links, and document the whole thing

**Files:**
- Modify: `internal/session/session.go` (`GC`, new `pruneGroups`)
- Test: `internal/session/session_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `Root()`, and the `groups/` layout from Task 6.
- Produces: nothing consumed later.

- [ ] **Step 1: Write the failing test**

Add to `internal/session/session_test.go`:

```go
// A group link outlives the session it names -- the session is collected by
// retention, the link is not -- so gc has to take them or the directory grows
// one entry per process group forever.
func TestGCPrunesDanglingGroupLinks(t *testing.T) {
	data := t.TempDir()
	t.Setenv("UNDO_DATA_DIR", data)

	live := mustSession(t, "rm live")
	writeJournal(t, live, "unlink\t/a\t-\tnone\n")

	groups := filepath.Join(data, "groups")
	if err := os.MkdirAll(groups, 0o700); err != nil {
		t.Fatal(err)
	}
	// one link to a session that exists, one to a session that does not
	if err := os.Symlink("../sessions/"+live.ID, filepath.Join(groups, "keyA")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../sessions/1111111111111111", filepath.Join(groups, "keyB")); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(groups, "keyA")); err != nil {
		t.Error("gc removed the group link of a session that still exists; the " +
			"group would start a second session while its first is still live")
	}
	if _, err := os.Lstat(filepath.Join(groups, "keyB")); err == nil {
		t.Error("gc kept a group link whose session is gone")
	}
}

// A stat that fails for a reason other than absence is not proof. Constructed
// with a regular file where a directory component would be, so stat through it
// returns ENOTDIR -- deterministic, and unlike a permissions trick it behaves
// the same when the tests run as root.
func TestGCKeepsAGroupLinkItCannotResolve(t *testing.T) {
	data := t.TempDir()
	t.Setenv("UNDO_DATA_DIR", data)
	sessions := filepath.Join(data, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "notadir"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	groups := filepath.Join(data, "groups")
	if err := os.MkdirAll(groups, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../sessions/notadir/child", filepath.Join(groups, "keyC")); err != nil {
		t.Fatal(err)
	}
	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(groups, "keyC")); err != nil {
		t.Error("gc removed a group link whose target it could not resolve; a " +
			"transient error is not proof the session is gone, and removing a " +
			"live mapping splits the group across two sessions")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/session/ -run TestGCPrunesDangling -v`
Expected: FAIL — `keyB` still exists.

- [ ] **Step 3: Implement**

In `internal/session/session.go`, add:

```go
// groupsDir holds one symlink per process group, naming the session that group
// writes to. It sits beside the sessions directory so it outlives any one of
// them.
func groupsDir() string {
	return filepath.Join(filepath.Dir(Root()), "groups")
}

// pruneGroups removes group links whose session is gone.
//
// A link is created before nothing and after the session directory it names,
// so a link that resolves to nothing is unambiguously stale rather than a
// session mid-creation. Removing a live one would be worse than leaving a
// stale one: the group would create a second session while its first is still
// being written.
func pruneGroups() {
	ents, err := os.ReadDir(groupsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		link := filepath.Join(groupsDir(), e.Name())
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue // only ever remove our own symlinks
		}
		// Only absence is proof. A transient error, a permission failure, or a
		// path that stopped resolving for any other reason is not evidence the
		// session is gone -- and removing a live mapping splits the group: its
		// next destructive call creates a second session while the first is
		// still being written.
		//
		// os.IsNotExist is exactly right here, not a near-miss for
		// errors.Is(err, syscall.ENOENT). Measured on this repo's Go
		// (1.24.13): stat through a non-directory component gives ENOTDIR, and
		// both os.IsNotExist and errors.Is(err, fs.ErrNotExist) are false for
		// it -- Errno.Is maps only ENOENT to fs.ErrNotExist. A review gate
		// flagged this as a bug; it was measured and rejected, and
		// TestGCKeepsAGroupLinkItCannotResolve is what keeps it honest.
		if _, err := os.Stat(link); !os.IsNotExist(err) {
			continue
		}
		os.Remove(link)
	}
}
```

and call it from `GC`, immediately before `return removed, nil`:

```go
	pruneGroups()
	return removed, nil
```

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ -v`
Expected: PASS.

Run: `test/in-container.sh make test`
Expected: PASS.

- [ ] **Step 5: Document it**

In `README.md`, in the environment-variable table, add:

```markdown
| `UNDO_ARM` | unset | `<pgid>:<starttime>` of the arming process, set by `undo arm`. Its presence lets the shim create sessions itself, with no shell hook. `UNDO_ARM=1` also works where only static environment can be set, but disables the armer-exclusion and detach tests |
| `UNDO_SID` | unset | terminal session id recorded by whoever set `UNDO_SESSION`. Lets the shim reject a session inherited across `setsid()` — a terminal multiplexer server, a daemon — which would otherwise be written to long after it finished |
| `UNDO_SESSION_MAX_AGE` | 21600 | seconds before a long-lived process group rolls to a new session (minimum 60). Only reached by daemons and multiplexer panes; an ordinary command never splits, because the age is measured from the group's own start |
```

Add a section after the shell-hook install instructions:

```markdown
### Capturing an agent's commands

A coding agent's tool call is `<shell> -c '...'`, which runs no `preexec` and
no `PROMPT_COMMAND`, so the shell hooks never fire and nothing is recorded.
Arm the agent instead:

    undo arm -- <your-agent>

Everything the agent runs through its shell is then captured, one session per
tool call, and `undo list` shows them like any other session. No shell hook is
involved and it works whatever shell the agent invokes, including `sh` and
`tcsh`.

`undo arm` arms an open-ended tree of commands; the agent itself is the armer
and is deliberately not captured, so it does not journal its own housekeeping.
**To capture one command, use `undo run -- <cmd>` instead** — running a single
command under `undo arm` puts it in the excluded group and records nothing.

Two limits worth knowing:

- **Programs that do not call libc are invisible.** Go binaries and static
  binaries issue raw syscalls, and so do parts of some JavaScript runtimes.
  An agent's own in-process file editing may therefore not be captured even
  when its shell commands are.
- **A long-lived process gets one session per `UNDO_SESSION_MAX_AGE`.** A
  terminal multiplexer pane or a daemon started by the agent is one process
  group for its whole life, so its session covers many unrelated commands.
  `undo -i` picks individual entries out of it.

#### Choosing what not to record

An armed agent journals its builds, test suites and package installs too. The
size budgets bound that, but two things are worth excluding deliberately:

    export UNDO_IGNORE=.venv:target:build:dist:.tox:.mypy_cache:.pytest_cache:vendor:site-packages

`undo` does not set this for you, and will not. Ignoring a path silently is how
you come to believe you are protected when you are not, so the list is yours to
choose; `undo doctor` prints the one in effect.

The second reason is not noise but secrecy. An agent that rewrites its own
credential or transcript files copies them into the store. The store is mode
700, but it is still a second copy in a second place — add the agent's own
state directory to `UNDO_IGNORE` unless you want it backed up.
```

- [ ] **Step 6: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/ README.md
git diff --cached --stat
git commit -m "gc: take the group links of sessions that are gone"
```

---

## Final verification

- [ ] `test/in-container.sh make test` — C unit harness, all Go tests, every
      e2e case through 49
- [ ] `test/in-container.sh --privileged test/multifs.sh` — the two-filesystem
      harnesses
- [ ] `test/in-container.sh bash -c 'apt-get install -y -qq zsh >/dev/null && make all >/dev/null && bash test/hook-zsh.sh'` — the zsh hook still loads. zsh is not in the test
      image; without installing it this exits 127 rather than skipping.
- [ ] Floor check from Global Constraints — still `GLIBC_2.34`
- [ ] `tools/check-no-site-data.sh && tools/check-ere.sh`
- [ ] `git fetch upstream && git log --oneline HEAD..upstream/main` — still
      current; upstream actively edits `shim/undo_shim.c`
- [ ] **Invariant 1, by hand as well as by case 45.** Run a destructive command
      armed, with `UNDO_DATA_DIR` pointing at an unwritable path, and confirm
      the command's exit status and `errno` are untouched. This is the one
      property whose failure is unrecoverable.

## Out of scope, deliberately

- **In-process agent writes.** Covered by phase 2 of the design — an
  `undo capture <path>...` verb driven from the harness's pre-tool hook. Not
  reachable by `LD_PRELOAD` on a runtime that issues raw syscalls, so nothing
  in this plan attempts it.
- **Login-scoped arming.** The same mechanism with the environment set in a
  login shell or node image. Gated on the administrative question in the
  design; `undo arm -- $SHELL` already works if someone chooses to.
- **A recommended `UNDO_IGNORE` shipped as a default.** The design is explicit
  that ignores stay documentation rather than silent behaviour. Doctor prints
  the effective list; nothing sets it.
