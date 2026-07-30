# undo multi-filesystem, Phase 2a — cross-device restore and distributed removal

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Go CLI correct when a backup lives on a different filesystem
from the file it protects — restoring it faithfully, and deleting it when the
session goes away — before the shim starts putting backups there.

**Architecture:** Three self-contained changes to `internal/restore` and
`internal/session`. `moveAny` grows a cross-device fallback that handles
symlinks, directories, and modes correctly; the two `os.Rename` sites that
bypass it are routed through it; and `Session.Remove` learns to delete backups
outside the session directory, gated on a containment check because those paths
come out of a journal. No shim change, so this lands independently of 2b.

**Tech Stack:** Go 1.24, bash (e2e and the two-filesystem harness), podman or
docker (Linux test environment).

## Global Constraints

Copied verbatim from `docs/design/undo-multifs-design.md` and `AGENTS.md`. Every
task inherits these.

- **The shim must never cause the user's command to fail.** All internal errors
  are swallowed; the real syscall's result is returned untouched. (No shim
  change in this plan, but the constraint stands.)
- **No new libc call may raise the glibc symbol floor** above `GLIBC_2.34`.
- **No site-specific data in this repository** — no hostnames, mount points,
  organization domains, internal addresses, usernames, or storage capacities.
- **The journal format is append-only and additive.** Fields may be appended to
  an op, never inserted or reordered. Journal indices are load-bearing:
  `restore.slot()` and `--only` are keyed by position, so records must never be
  filtered out of the parsed entry list.
- The tool is Linux-only and the workstation is macOS. **Every build and test
  runs through `test/in-container.sh`**; `test/multifs.sh` also needs
  `--privileged`.
- The Go module path is `github.com/edaywalid/undo`. Do not rename it.

## Environment note

`test/in-container.sh` copies the working tree into the container and builds
there. It does not write back, so anything a test produces is discarded — which
is why every assertion has to run inside the same invocation that builds.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/restore/restore.go` | modify | `moveAny` fallback split into `copyAcross`; `copyFile`/`copyTree` helpers; two `OpMkdir` sites routed through `moveAny` |
| `internal/restore/restore_test.go` | modify (append) | unit coverage for the fallback, called directly so it needs no second filesystem |
| `internal/session/session.go` | modify | `backupPaths`, `inStore`, `removeDistributedBackups`; `Remove` calls them |
| `internal/session/session_test.go` | modify (append) | distributed removal, and the containment guard refusing a path outside a store |
| `test/multifs-restore.sh` | create | the genuine EXDEV proof: store on one tmpfs, files on the other |

## Interfaces produced by this plan

Later plans depend on these exact names and signatures.

```go
// internal/restore
func moveAny(src, dst string) error        // unchanged signature
func copyAcross(src, dst string) error     // new: moveAny's fallback, testable alone
func copyFile(src, dst string, fi os.FileInfo) error
func copyTree(src, dst string) error

// internal/session
func (s *Session) Remove() error           // unchanged signature
func (s *Session) backupPaths() []string   // absolute backup paths named by the journal
func inStore(path, id string) bool         // path sits directly inside <root>/.undo/<id>
```

Plan 2b adds `storemv` resolution inside `session.load`, so by the time
`Remove` reads `s.Entries` the paths are already corrected. Nothing in this
plan needs to know that.

---

### Task 1: `copyAcross` — a cross-device fallback that preserves what it moves

`moveAny`'s current fallback opens `src` with `os.Open`, which follows
symlinks, and creates `dst` with `os.OpenFile`, whose mode argument is masked
by the umask. On one filesystem none of this shows, because `os.Rename`
succeeds and the fallback never runs. Once the store is elsewhere, a restored
symlink silently becomes a regular file holding its target's bytes, and modes
drift by the umask.

Splitting the fallback into its own function is what makes it testable: a unit
test can call `copyAcross` directly and get the cross-device behavior without
needing two filesystems.

**Files:**
- Modify: `internal/restore/restore.go:45-76`
- Test: `internal/restore/restore_test.go` (append)

**Interfaces:**
- Consumes: nothing (first task)
- Produces: `copyAcross(src, dst string) error`, `copyFile(src, dst string, fi os.FileInfo) error`, `copyTree(src, dst string) error`. `moveAny` keeps its signature and delegates.

- [ ] **Step 1: Write the failing tests**

Append to `internal/restore/restore_test.go`:

```go
func TestCopyAcrossPreservesSymlink(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "target.txt")
	write(t, target, "pointed at")
	link := filepath.Join(work, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "moved")

	if err := copyAcross(link, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("a symlink was restored as a regular file")
	}
	got, err := os.Readlink(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("link target = %q, want %q", got, target)
	}
	if present(link) {
		t.Error("source should be gone after a move")
	}
}

func TestCopyAcrossPreservesMode(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "script")
	write(t, src, "#!/bin/sh\n")
	if err := os.Chmod(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "moved")

	if err := copyAcross(src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// OpenFile's mode argument is masked by the umask, so this fails unless
	// the mode is set explicitly afterwards.
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", fi.Mode().Perm())
	}
}

func TestCopyAcrossMovesDirectoryTree(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "tree")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "top.txt"), "top")
	write(t, filepath.Join(src, "sub", "deep.txt"), "deep")
	if err := os.Symlink("top.txt", filepath.Join(src, "rel")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(work, "parked")

	if err := copyAcross(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "top.txt")); got != "top" {
		t.Errorf("top.txt = %q", got)
	}
	if got := read(t, filepath.Join(dst, "sub", "deep.txt")); got != "deep" {
		t.Errorf("sub/deep.txt = %q", got)
	}
	fi, err := os.Lstat(filepath.Join(dst, "rel"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink inside the tree was dereferenced")
	}
	if present(src) {
		t.Error("source tree should be gone after a move")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh go test ./internal/restore/ -run TestCopyAcross -v`

Expected: compile failure — `undefined: copyAcross`. That is the correct
failure; the function does not exist yet.

- [ ] **Step 3: Implement**

In `internal/restore/restore.go`, replace `moveAny` (lines 45-76) with:

```go
// moveAny renames src onto dst, falling back to a copy when the two are on
// different filesystems.
func moveAny(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyAcross(src, dst)
}

// copyAcross reproduces src at dst and then removes src. It is moveAny's
// cross-device fallback, split out so it can be tested without two
// filesystems mounted.
//
// os.Open follows symlinks and os.OpenFile's mode is masked by the umask, so
// a naive copy turns a symlink into a regular file and drifts permissions.
// Neither shows while the store shares a filesystem with its files, because
// then os.Rename always succeeds and this never runs.
func copyAcross(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	case fi.IsDir():
		if err := copyTree(src, dst); err != nil {
			return err
		}
	case fi.Mode().IsRegular():
		if err := copyFile(src, dst, fi); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cannot move %s across filesystems: %s is not a regular file, directory, or symlink",
			src, fi.Mode().Type())
	}
	return os.RemoveAll(src)
}

// copyFile writes src's contents, mode and mtime to dst.
func copyFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// OpenFile's mode is masked by the umask; set it explicitly.
	if err := os.Chmod(dst, fi.Mode().Perm()); err != nil {
		return err
	}
	// Timestamps are a nicety: filesystems that refuse them should not fail
	// the restore.
	os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	return nil
}

// copyTree recursively reproduces the directory src at dst.
func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	// Create it traversable first; the real mode goes on at the end, since a
	// read-only or non-executable directory cannot be populated.
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		efi, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case efi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(s)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, d); err != nil {
				return err
			}
		case efi.IsDir():
			if err := copyTree(s, d); err != nil {
				return err
			}
		case efi.Mode().IsRegular():
			if err := copyFile(s, d, efi); err != nil {
				return err
			}
		default:
			return fmt.Errorf("cannot copy %s: %s is not a regular file, directory, or symlink",
				s, efi.Mode().Type())
		}
	}
	return os.Chmod(dst, fi.Mode().Perm())
}
```

`fmt` and `io` are already imported. No new imports are needed.

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/restore/ -v`

Expected: PASS, including the three new cases and every pre-existing one.

- [ ] **Step 5: Commit**

```bash
git add internal/restore/restore.go internal/restore/restore_test.go
git commit -m 'restore: preserve symlinks, modes, and trees across filesystems

moveAny fell back to os.Open + io.Copy when os.Rename failed, which is what
happens once a backup lives on a different filesystem from the file it
protects. os.Open follows symlinks, so a restored symlink became a regular
file holding its target'"'"'s bytes; os.OpenFile'"'"'s mode argument is masked by
the umask, so permissions drifted; and io.Copy on a directory fails EISDIR,
so a directory could not cross at all.

Split the fallback into copyAcross so it can be tested directly rather than
only through a second mounted filesystem, and teach it the three cases that
actually occur.'
```

---

### Task 2: Route the two `OpMkdir` renames through `moveAny`

`restore.go:298` parks a directory into the session store when `undo --force`
finds it non-empty, and `:309` moves it back on redo. Both call `os.Rename`
directly, so both fail `EXDEV` the moment the session store is on another
filesystem — which is already true today whenever `UNDO_DATA_DIR` is on a
different mount from the working tree, and will be the normal case after 2b.

These are the only two genuine bypasses. `swapAny`'s two `os.Rename` calls move
between `a` and `a + ".undo-swap"`, which are in the same directory by
construction and therefore cannot return `EXDEV`; leave them alone.

**Files:**
- Modify: `internal/restore/restore.go:298`, `internal/restore/restore.go:309`
- Test: `internal/restore/restore_test.go` (append)

**Interfaces:**
- Consumes: `moveAny`, `copyAcross`, `copyTree` from Task 1.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `internal/restore/restore_test.go`:

```go
func TestMkdirForceParksAndRestoresPopulatedDirectory(t *testing.T) {
	work := t.TempDir()
	made := filepath.Join(work, "made")
	if err := os.MkdirAll(filepath.Join(made, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(made, "sub", "kept.txt"), "do not lose me")
	s := newSession(t, []journal.Entry{{Op: journal.OpMkdir, Fields: []string{made}}})

	// undo: the directory is not empty, so --force parks it in the store
	res, err := Run(s, Undo, Options{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 {
		t.Fatalf("Done = %d, want 1 (skipped: %v)", res.Done, res.Skipped)
	}
	if present(made) {
		t.Fatal("undo --force should have moved the directory aside")
	}
	parked := filepath.Join(s.Dir, "data", "undo-0", "sub", "kept.txt")
	if got := read(t, parked); got != "do not lose me" {
		t.Fatalf("parked content = %q", got)
	}

	// redo: it comes back with its contents
	if _, err := Run(s, Redo, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(made, "sub", "kept.txt")); got != "do not lose me" {
		t.Fatalf("after redo = %q", got)
	}
}
```

- [ ] **Step 2: Run it**

Run: `test/in-container.sh go test ./internal/restore/ -run TestMkdirForce -v`

Expected: PASS. Both directories are on one filesystem here, so `os.Rename`
still succeeds — this test pins the behavior that must survive the change, and
the cross-device case is proved in Task 4. If it fails now, something else is
wrong and Step 3 will not fix it.

- [ ] **Step 3: Make the change**

In `internal/restore/restore.go`, in the `journal.OpMkdir` case, replace the
two raw renames.

Line 298, in the `Undo` branch:

```go
				if err = os.Remove(field(0)); err != nil {
					if !opts.Force {
						skip("directory not empty, use --force to move it aside")
						continue
					}
					err = moveAny(field(0), slot(s, i))
				}
```

Line 309, in the `Redo` branch:

```go
				if exists(slot(s, i)) {
					err = moveAny(slot(s, i), field(0))
				} else {
					err = os.Mkdir(field(0), 0o755)
				}
```

Both are a one-word change from `os.Rename` to `moveAny`.

- [ ] **Step 4: Run the full unit suite**

Run: `test/in-container.sh go test ./... -v`

Expected: PASS throughout.

- [ ] **Step 5: Commit**

```bash
git add internal/restore/restore.go internal/restore/restore_test.go
git commit -m "restore: move parked directories with moveAny, not os.Rename

The two OpMkdir sites park a non-empty directory into the session store on
undo --force and bring it back on redo. Both called os.Rename directly, so
both failed EXDEV as soon as the store was on a different filesystem from
the directory -- which UNDO_DATA_DIR can already arrange today, and which
filesystem-local stores make the normal case.

swapAny's renames are deliberately left alone: they move between a and
a+\".undo-swap\" in one directory, so they cannot return EXDEV."
```

---

### Task 3: `Session.Remove` deletes backups outside the session directory

`Remove` is `os.RemoveAll(s.Dir)`. Once the shim puts backups on the
filesystem of the file they protect, that reclaims nothing: `undo gc`, `undo
purge`, and `undo uninstall --purge` all funnel through this one method, so
all three would silently leak. Teaching `Remove` fixes all three.

The backup paths come out of the journal, which is a file in the user's data
directory — untrusted input for this purpose. A truncated, corrupted, or
hand-edited journal must not turn `undo purge` into an arbitrary `rm`. Every
deletion is therefore gated on the path sitting directly inside a `.undo`
directory named for this session.

**Files:**
- Modify: `internal/session/session.go:289-292`
- Test: `internal/session/session_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `(*Session).backupPaths() []string`, `inStore(path, id string) bool`, `(*Session).removeDistributedBackups()`. `Remove`'s signature is unchanged, so `GC` and `cmdPurge` need no edit.

- [ ] **Step 1: Write the failing tests**

Append to `internal/session/session_test.go`:

```go
func TestRemoveDeletesDistributedBackups(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm -rf project")
	if err != nil {
		t.Fatal(err)
	}

	// a backup the way the shim will place it once stores are per-filesystem
	vol := t.TempDir()
	store := filepath.Join(vol, ".undo", s.ID)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(store, "1234-1")
	if err := os.WriteFile(backup, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "gone.txt")+"\t"+backup+"\n")

	reloaded, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Error("the distributed backup was left behind")
	}
	if _, err := os.Lstat(store); !os.IsNotExist(err) {
		t.Error("the empty store directory was left behind")
	}
	if _, err := os.Lstat(filepath.Join(vol, ".undo")); !os.IsNotExist(err) {
		t.Error("the empty .undo directory was left behind")
	}
	if _, err := os.Lstat(s.Dir); !os.IsNotExist(err) {
		t.Error("the session directory was left behind")
	}
}

func TestRemoveRefusesPathsOutsideAStore(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("something")
	if err != nil {
		t.Fatal(err)
	}
	vol := t.TempDir()
	precious := filepath.Join(vol, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A journal naming an arbitrary path as its backup. Nothing about this is
	// exotic: a truncated write or a hand-edited journal produces it.
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "gone.txt")+"\t"+precious+"\n")

	reloaded, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(precious); err != nil {
		t.Fatal("purge deleted a path that was not inside a store")
	}
}

func TestRemoveLeavesAnotherSessionsStoreAlone(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	mine, err := Create("mine")
	if err != nil {
		t.Fatal(err)
	}
	vol := t.TempDir()
	// a store belonging to a different session, sharing the same .undo parent
	other := filepath.Join(vol, ".undo", "9999999999999999")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	otherBackup := filepath.Join(other, "1-1")
	if err := os.WriteFile(otherBackup, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, mine, "unlink\t"+filepath.Join(vol, "x.txt")+"\t"+otherBackup+"\n")

	reloaded, err := Get(mine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(otherBackup); err != nil {
		t.Fatal("removing one session deleted another session's backup")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `test/in-container.sh go test ./internal/session/ -run TestRemove -v`

Expected: `TestRemoveDeletesDistributedBackups` FAILS with "the distributed
backup was left behind". The other two PASS already, because today's `Remove`
touches nothing outside `s.Dir` — they are there to stay passing after Step 3,
which is the whole point of them.

- [ ] **Step 3: Implement**

In `internal/session/session.go`, replace `Remove` (lines 289-292) with:

```go
// backupPaths returns the backup locations this session's journal names.
// The shim records them as absolute paths, so they may be anywhere.
func (s *Session) backupPaths() []string {
	var out []string
	for _, e := range s.Entries {
		var p string
		switch e.Op {
		case journal.OpUnlink, journal.OpMod:
			if len(e.Fields) > 1 {
				p = e.Fields[1]
			}
		case journal.OpRename:
			if len(e.Fields) > 2 {
				p = e.Fields[2]
			}
		}
		if p == "" || p == "-" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// inStore reports whether path sits directly inside a store directory
// belonging to session id -- that is, whether it looks like
// <root>/.undo/<id>/<name>.
//
// Backup paths come from the journal, which for this purpose is untrusted:
// a truncated write, a corrupted file, or a hand edit can name any path at
// all, and purge must not turn into an arbitrary rm. This is the check that
// makes deleting a journal-supplied path safe, so it is deliberately strict
// and structural rather than a prefix test.
func inStore(path, id string) bool {
	if id == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	dir := filepath.Dir(path)
	return filepath.Base(dir) == id && filepath.Base(filepath.Dir(dir)) == ".undo"
}

// removeDistributedBackups deletes the backups the shim placed outside the
// session directory, on the filesystem of the file each one protects, then
// removes the store directories they leave empty.
//
// Best-effort on purpose. A backup on a filesystem that is not mounted right
// now cannot be removed, and refusing to drop the session over it would leave
// a session nothing can ever delete. The gc orphan sweep is the backstop for
// exactly that case.
func (s *Session) removeDistributedBackups() {
	prefix := s.Dir + string(os.PathSeparator)
	stores := make(map[string]bool)
	for _, p := range s.backupPaths() {
		if strings.HasPrefix(p, prefix) {
			continue // inside the session dir; RemoveAll gets it
		}
		if !inStore(p, s.ID) {
			continue // not ours to delete
		}
		os.Remove(p)
		stores[filepath.Dir(p)] = true
	}
	for d := range stores {
		// Both only succeed when empty, which is what keeps a store shared
		// with another session, and a .undo shared with another store, intact.
		os.Remove(d)
		os.Remove(filepath.Dir(d))
	}
}

// Remove deletes a session and its backups entirely, including the ones held
// on other filesystems. The journal names those, so it has to go last.
func (s *Session) Remove() error {
	s.removeDistributedBackups()
	return os.RemoveAll(s.Dir)
}
```

`filepath`, `os`, `strings`, and `journal` are already imported.

- [ ] **Step 4: Run the tests**

Run: `test/in-container.sh go test ./internal/session/ -v`

Expected: PASS, all three new cases plus every pre-existing one.

- [ ] **Step 5: Run everything**

Run: `test/in-container.sh make test`

Expected: `go test ./...` passes and `test/e2e.sh` reports `all cases passed`.
Case 15 (`gc removes empty sessions, purge empties the store`) is the one most
likely to notice a regression here.

- [ ] **Step 6: Commit**

```bash
git add internal/session/session.go internal/session/session_test.go
git commit -m 'session: delete backups that live outside the session directory

Remove was os.RemoveAll(s.Dir), which reclaims nothing once the shim starts
placing backups on the filesystem of the file they protect. gc, purge, and
uninstall --purge all funnel through this one method, so all three would
have leaked silently.

Backup paths come out of the journal, and for deletion purposes that is
untrusted input -- a truncated or hand-edited journal must not turn purge
into an arbitrary rm. Every deletion is gated on inStore(), which requires
the path to sit directly inside a .undo directory named for this session.
The store and .undo directories are then removed only if empty, so a store
shared with another session survives.

Best-effort by design: a backup on an unmounted volume cannot be removed,
and blocking the session on it would make the session undeletable. The gc
orphan sweep in 2c is the backstop.'
```

---

### Task 4: Prove it on two real filesystems

Everything above is unit-tested against a single filesystem, with `copyAcross`
called directly to reach the fallback. That is a proxy. This task exercises the
real thing: `os.Rename` genuinely returning `EXDEV`, with the session store on
one tmpfs and the files on another.

No shim change is needed to arrange this — `UNDO_DATA_DIR` already puts the
store wherever we want, which is exactly the cross-device configuration 2b
makes automatic.

**Files:**
- Create: `test/multifs-restore.sh`

**Interfaces:**
- Consumes: `test/multifs.sh` from Phase 1, which exports `FS_A` and `FS_B` and runs its argument as an assertion script.
- Produces: `test/multifs-restore.sh`, runnable as `test/multifs.sh test/multifs-restore.sh`.

- [ ] **Step 1: Write the assertion script**

Create `test/multifs-restore.sh`:

```bash
#!/usr/bin/env bash
# Cross-device restore: the session store on one filesystem, the files on
# another, so every os.Rename in the restore path genuinely returns EXDEV.
#
#   test/in-container.sh --privileged bash -c \
#       'make && test/multifs.sh test/multifs-restore.sh'
#
# Sourced by test/multifs.sh, which exports FS_A and FS_B.
set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }

ROOT=$(pwd)
UNDO=$ROOT/bin/undo
[[ -x $UNDO ]] || fail "bin/undo not built; run make first"

# store on B, files on A: every restore crosses a filesystem boundary
export UNDO_DATA_DIR=$FS_B/undo-data
WORK=$FS_A/work
mkdir -p "$WORK" "$UNDO_DATA_DIR"

# Build each session by hand rather than through the shim: this is a Go-side
# test, and driving it directly keeps it independent of shim behavior.
#
# `undo <id>` is not a command -- a bare argument falls through to the usage
# text and exit 2. The spellings below (apply / redo / purge) are the real ones.
n=0
new_session() { # new_session <journal-line...>  -> sets $sdir
    n=$((n + 1))
    sid=$(printf '17%08d%06d' "$n" "$n")
    sdir=$UNDO_DATA_DIR/sessions/$sid
    mkdir -p "$sdir/data"
    printf 'synthetic cross-device session %s\n' "$n" >"$sdir/cmd"
    : >"$sdir/done"
}

echo "== cross-device: a created symlink survives undo and redo as a symlink"
new_session
ln -s /etc/hostname "$WORK/alink"
printf 'create\t%s\n' "$WORK/alink" >"$sdir/journal"
"$UNDO" apply "$sid" -y >/dev/null || fail "undo of the created symlink failed"
[[ ! -e $WORK/alink && ! -L $WORK/alink ]] || fail "undo did not remove the symlink"
[[ -L $sdir/data/undo-0 ]] ||
    fail "a symlink parked across filesystems is no longer a symlink"
"$UNDO" redo "$sid" -y >/dev/null || fail "redo of the created symlink failed"
[[ -L $WORK/alink ]] || fail "a symlink restored across filesystems is not a symlink"
[[ $(readlink "$WORK/alink") == /etc/hostname ]] ||
    fail "restored symlink points at $(readlink "$WORK/alink")"

echo "== cross-device: an executable keeps its mode"
new_session
printf '#!/bin/sh\n' >"$sdir/data/b2"
chmod 755 "$sdir/data/b2"
printf 'unlink\t%s\t%s\n' "$WORK/script" "$sdir/data/b2" >"$sdir/journal"
"$UNDO" apply "$sid" -y >/dev/null || fail "undo of the script failed"
mode=$(stat -c %a "$WORK/script")
[[ $mode == 755 ]] || fail "mode across filesystems = $mode, want 755"

echo "== cross-device: undo --force parks a populated directory"
new_session
mkdir -p "$WORK/made/sub"
echo "do not lose me" >"$WORK/made/sub/kept.txt"
printf 'mkdir\t%s\n' "$WORK/made" >"$sdir/journal"
"$UNDO" apply "$sid" -y --force >/dev/null || fail "forced undo of the mkdir failed"
[[ ! -e $WORK/made ]] || fail "the directory was not moved aside"
[[ $(cat "$sdir/data/undo-0/sub/kept.txt") == "do not lose me" ]] ||
    fail "the parked tree lost its contents crossing a filesystem"

echo "== cross-device: redo brings the directory back"
"$UNDO" redo "$sid" -y >/dev/null || fail "redo failed"
[[ $(cat "$WORK/made/sub/kept.txt") == "do not lose me" ]] ||
    fail "the tree did not come back intact"

echo "== cross-device: purge reclaims a store on the other filesystem"
new_session
store=$FS_A/.undo/$sid
mkdir -p "$store"
echo saved >"$store/1-1"
printf 'unlink\t%s\t%s\n' "$WORK/gone.txt" "$store/1-1" >"$sdir/journal"
"$UNDO" purge -y >/dev/null || fail "purge failed"
[[ ! -e $store/1-1 ]] || fail "purge left a backup on the other filesystem"
[[ ! -e $FS_A/.undo ]] || fail "purge left an empty .undo behind"

echo
echo "cross-device restore ok"
```

```bash
chmod +x test/multifs-restore.sh
```

- [ ] **Step 2: Run it**

Run:

```bash
test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-restore.sh'
```

Expected: the four harness assertions from Phase 1, then all five
`== cross-device:` lines, then `cross-device restore ok`.

The CLI spellings used here were read off `cmd/undo/main.go:255-351` and are
exact: `undo apply <id>`, `undo redo <id>`, `undo purge`, all taking `-y` and
`--force`. A bare `undo <id>` is **not** a command — it falls through to the
usage text and exits 2. If a step fails, fix the script, not the CLI: this plan
changes no user-facing behavior.

- [ ] **Step 3: Prove it was actually cross-device**

A test that silently ran on one filesystem would pass for the wrong reason.
Confirm the two paths really differ:

```bash
test/in-container.sh --privileged bash -c '
  make >/dev/null
  . test/multifs.sh </dev/null || true
  stat -c "%d %n" /mnt/undo-fs-a /mnt/undo-fs-b'
```

Expected: two different device numbers. If they match, the mounts did not
happen and every assertion above proved nothing.

- [ ] **Step 4: Scan and commit**

```bash
tools/check-no-site-data.sh test/multifs-restore.sh && echo "scan clean"
git add test/multifs-restore.sh
git commit -m 'test: cross-device restore on two real filesystems

The unit tests reach the copy fallback by calling copyAcross directly, which
is a proxy for a filesystem boundary rather than the thing itself. This puts
the session store on one tmpfs and the files on another, so os.Rename really
returns EXDEV, and asserts the four properties that silently degrade without
it: a symlink stays a symlink, a mode survives the umask, a populated
directory can be parked and brought back, and purge reclaims a store on the
far side.'
```

---

## Definition of done

- [ ] `test/in-container.sh make test` passes: `go test ./...` green, `all cases passed`
- [ ] `test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-restore.sh'` prints `cross-device restore ok`
- [ ] The two tmpfs mounts are confirmed to have distinct `st_dev`, so the cross-device assertions could not have passed on one filesystem
- [ ] A symlink restored across a filesystem boundary is still a symlink
- [ ] A 0755 backup restored across a boundary is still 0755 under a 022 umask
- [ ] `undo --force` parks and `undo redo` restores a populated directory across a boundary
- [ ] `Session.Remove` deletes a backup under `<root>/.undo/<id>/` and removes the emptied store and `.undo` directories
- [ ] `Session.Remove` leaves a journal-named path that is not inside a store untouched
- [ ] `Session.Remove` leaves another session's store untouched
- [ ] `git ls-files -z | xargs -0 tools/check-no-site-data.sh` exits 0

## Deliberately not in this plan

- Filesystem-local store placement, the containment guard, the resolver cache, and the rmdir relocation policy (2b)
- Recording the save method in the journal, and the `storemv` record (2b)
- GC accounting by method, `UNDO_MAX_AGE`, and the orphan sweep (2c)
- `FICLONE`, the copy ladder, the visible `lost` report, per-volume `doctor` (phase 3)

## Notes for the implementer

- **`swapAny` is not a bug.** An earlier revision of the design listed its
  `os.Rename` calls as needing conversion. They move between `a` and
  `a + ".undo-swap"`, which are in one directory, so `EXDEV` is impossible.
  Upstream reached the same conclusion for `OpExchange` in `ec4de57`. Do not
  "fix" them.
- **Do not change `moveAny`'s signature.** Eleven call sites use it, and Task 2
  adds two more. Keeping it stable is what makes this plan a small diff.
- `copyTree` creates each directory `0700` and sets the real mode at the end,
  on purpose: a mode like `0555` applied first makes the directory impossible
  to populate, and the failure would look like a permissions bug rather than an
  ordering one.
- The `default:` arms in `copyAcross` and `copyTree` return an error rather than
  skipping. A device node, socket, or fifo in a parked tree is rare enough that
  failing loudly beats a restore that quietly drops entries.
