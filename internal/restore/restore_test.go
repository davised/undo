package restore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edaywalid/undo/internal/journal"
	"github.com/edaywalid/undo/internal/session"
)

// newSession builds a session in a temp dir with a data/ subdir, ready
// to hold backups and receive the undone/done markers.
func newSession(t *testing.T, entries []journal.Entry) *session.Session {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &session.Session{ID: "test", Dir: dir, Entries: entries}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func present(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestUndoUnlinkRestoresBackup(t *testing.T) {
	work := t.TempDir()
	victim := filepath.Join(work, "gone.txt")
	s := newSession(t, nil)
	backup := filepath.Join(s.Dir, "data", "b1")
	write(t, backup, "the original bytes")
	s.Entries = []journal.Entry{{Op: journal.OpUnlink, Fields: []string{victim, backup}}}

	res, err := Run(s, Undo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 {
		t.Fatalf("Done = %d, want 1 (skipped: %v)", res.Done, res.Skipped)
	}
	if got := read(t, victim); got != "the original bytes" {
		t.Errorf("restored content = %q", got)
	}
	if !s.Undone {
		t.Error("session should be marked undone")
	}
}

func TestUndoUnlinkConflictSkipsWithoutForce(t *testing.T) {
	work := t.TempDir()
	victim := filepath.Join(work, "back.txt")
	write(t, victim, "recreated since") // path exists again
	s := newSession(t, nil)
	backup := filepath.Join(s.Dir, "data", "b1")
	write(t, backup, "old")
	s.Entries = []journal.Entry{{Op: journal.OpUnlink, Fields: []string{victim, backup}}}

	res, _ := Run(s, Undo, Options{})
	if res.Done != 0 || len(res.Skipped) != 1 {
		t.Fatalf("want 0 done / 1 skipped, got %d done / %d skipped", res.Done, len(res.Skipped))
	}
	if got := read(t, victim); got != "recreated since" {
		t.Errorf("clobbered without force: %q", got)
	}

	// with force it overwrites
	res, _ = Run(s, Undo, Options{Force: true})
	if res.Done != 1 || read(t, victim) != "old" {
		t.Errorf("force undo failed: done=%d content=%q", res.Done, read(t, victim))
	}
}

func TestModSwapsBothDirections(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "config.yaml")
	write(t, file, "new")
	s := newSession(t, nil)
	backup := filepath.Join(s.Dir, "data", "b1")
	write(t, backup, "old")
	s.Entries = []journal.Entry{{Op: journal.OpMod, Fields: []string{file, backup}}}

	if _, err := Run(s, Undo, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, file); got != "old" {
		t.Fatalf("after undo = %q, want old", got)
	}
	if _, err := Run(s, Redo, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, file); got != "new" {
		t.Fatalf("after redo = %q, want new", got)
	}
}

func TestCreateUndoRedoRoundTrip(t *testing.T) {
	work := t.TempDir()
	made := filepath.Join(work, "made.txt")
	write(t, made, "accidental")
	s := newSession(t, []journal.Entry{{Op: journal.OpCreate, Fields: []string{made}}})

	if _, err := Run(s, Undo, Options{}); err != nil {
		t.Fatal(err)
	}
	if present(made) {
		t.Fatal("undo should have removed the created file")
	}
	if _, err := Run(s, Redo, Options{}); err != nil {
		t.Fatal(err)
	}
	if !present(made) || read(t, made) != "accidental" {
		t.Fatal("redo should have restored the created file with its content")
	}
}

func TestRenameUndoRestoresBothSides(t *testing.T) {
	work := t.TempDir()
	oldp := filepath.Join(work, "a.txt")
	newp := filepath.Join(work, "b.txt")
	write(t, newp, "moved content") // mv a -> b already happened
	s := newSession(t, nil)
	// b previously held content that mv clobbered; the shim saved it
	backup := filepath.Join(s.Dir, "data", "b1")
	write(t, backup, "clobbered original of b")
	s.Entries = []journal.Entry{{Op: journal.OpRename, Fields: []string{oldp, newp, backup}}}

	if _, err := Run(s, Undo, Options{}); err != nil {
		t.Fatal(err)
	}
	if read(t, oldp) != "moved content" {
		t.Errorf("a.txt not restored: %q", read(t, oldp))
	}
	if read(t, newp) != "clobbered original of b" {
		t.Errorf("b.txt backup not restored: %q", read(t, newp))
	}
}

func TestRmdirUndoRecreatesDirectory(t *testing.T) {
	work := t.TempDir()
	gone := filepath.Join(work, "sub")
	s := newSession(t, []journal.Entry{
		{Op: journal.OpRmdir, Fields: []string{gone, "755"}},
	})
	if _, err := Run(s, Undo, Options{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(gone)
	if err != nil || !fi.IsDir() {
		t.Fatal("undo should have recreated the directory")
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	work := t.TempDir()
	victim := filepath.Join(work, "gone.txt")
	s := newSession(t, nil)
	backup := filepath.Join(s.Dir, "data", "b1")
	write(t, backup, "x")
	s.Entries = []journal.Entry{{Op: journal.OpUnlink, Fields: []string{victim, backup}}}

	res, _ := Run(s, Undo, Options{DryRun: true})
	if present(victim) {
		t.Error("dry run restored a file")
	}
	if len(res.Actions) != 1 {
		t.Errorf("dry run should list 1 action, got %d", len(res.Actions))
	}
	if s.Undone {
		t.Error("dry run should not mark the session undone")
	}
}

func TestExchange(t *testing.T) {
	work := t.TempDir()
	a := filepath.Join(work, "a")
	b := filepath.Join(work, "b")
	write(t, a, "AAA")
	write(t, b, "BBB")
	s := newSession(t, []journal.Entry{
		{Op: journal.OpExchange, Fields: []string{a, b}},
	})

	if _, err := Run(s, Undo, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, a); got != "BBB" {
		t.Fatalf("a = %q, want BBB", got)
	}
	if got := read(t, b); got != "AAA" {
		t.Fatalf("b = %q, want AAA", got)
	}
}

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

func TestSelectiveReplayLeavesStateAndOthers(t *testing.T) {
	work := t.TempDir()
	f1 := filepath.Join(work, "one.txt")
	f2 := filepath.Join(work, "two.txt")
	s := newSession(t, nil)
	b1 := filepath.Join(s.Dir, "data", "b1")
	b2 := filepath.Join(s.Dir, "data", "b2")
	write(t, b1, "one")
	write(t, b2, "two")
	s.Entries = []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{f1, b1}},
		{Op: journal.OpUnlink, Fields: []string{f2, b2}},
	}

	// cherry-pick only entry index 0
	res, _ := Run(s, Undo, Options{Only: map[int]bool{0: true}})
	if res.Done != 1 {
		t.Fatalf("Done = %d, want 1", res.Done)
	}
	if !present(f1) || present(f2) {
		t.Error("only the selected entry should have been restored")
	}
	if s.Undone {
		t.Error("selective replay must not change session state")
	}
}

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

func TestMoveAnyRefusesToMergeIntoExistingDirectory(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "a.txt"), "from src")
	dst := filepath.Join(work, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dst, "keep.txt"), "already here")

	// Same filesystem, so this rename fails ENOTEMPTY rather than EXDEV. The
	// copy fallback must not treat that as "cross-device" and merge the trees.
	if err := moveAny(src, dst); err == nil {
		t.Fatal("moveAny merged src into a non-empty destination and reported success")
	}
	if got := read(t, filepath.Join(dst, "keep.txt")); got != "already here" {
		t.Errorf("destination content disturbed: %q", got)
	}
	if !present(src) {
		t.Error("source was removed even though the move failed")
	}
}

func TestCopyAcrossRefusesNonEmptyDestinationDirectory(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "a.txt"), "from src")
	dst := filepath.Join(work, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dst, "stale.txt"), "left over from an earlier failure")

	// This is the cross-device path: os.Rename would have returned EXDEV, so
	// copyAcross runs. It must refuse a non-empty destination for the same
	// reason rename does -- a move that silently unions two trees and reports
	// success is worse than one that fails.
	if err := copyAcross(src, dst); err == nil {
		t.Fatal("copyAcross merged into a non-empty destination and reported success")
	}
	if !present(src) {
		t.Error("source was removed even though the move failed")
	}
	if !present(filepath.Join(dst, "stale.txt")) {
		t.Error("pre-existing destination content was disturbed")
	}
}

// A rename that overwrote a file, whose backup was discarded when its store
// was destroyed. The backup field reads "-" after ResolveStoreMoves, but the
// method field still says a backup existed -- so this is not the same as a
// rename that never needed one, and must not be replayed as if it were.
func TestRenameWithDiscardedBackupIsReportedNotHalfDone(t *testing.T) {
	work := t.TempDir()
	oldp := filepath.Join(work, "a.txt")
	newp := filepath.Join(work, "b.txt")
	write(t, newp, "moved content")
	s := newSession(t, []journal.Entry{
		{Op: journal.OpRename, Fields: []string{oldp, newp, "-", "link"}},
	})

	res, err := Run(s, Undo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 0 || len(res.Skipped) != 1 {
		t.Fatalf("want 0 done / 1 skipped, got %d done / %d skipped: %v",
			res.Done, len(res.Skipped), res.Skipped)
	}
	if !present(newp) {
		t.Error("the rename was half-reverted, leaving the overwritten file gone for good")
	}
}

// The genuine no-backup rename -- nothing was overwritten -- must still revert.
func TestRenameWithoutBackupStillReverts(t *testing.T) {
	work := t.TempDir()
	oldp := filepath.Join(work, "a.txt")
	newp := filepath.Join(work, "b.txt")
	write(t, newp, "moved content")
	s := newSession(t, []journal.Entry{
		{Op: journal.OpRename, Fields: []string{oldp, newp, "-", "none"}},
	})

	res, err := Run(s, Undo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done != 1 {
		t.Fatalf("Done = %d, want 1 (skipped: %v)", res.Done, res.Skipped)
	}
	if read(t, oldp) != "moved content" {
		t.Error("a rename with no overwritten target did not revert")
	}
}
