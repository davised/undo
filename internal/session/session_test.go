package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeJournal(t *testing.T, s *Session, line string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, "journal"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndList(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("fresh session with our own pid should be live")
	}
	if err := s.MarkDone(); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, s, "create\t/tmp/x\n")

	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Cmd != "rm x" || all[0].Live() {
		t.Fatalf("unexpected list: %+v", all)
	}
}

func TestGCRemovesEmptyAndOversized(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())

	old, _ := Create("big command")
	old.MarkDone()
	writeJournal(t, old, "create\t/tmp/big\n")
	if err := os.WriteFile(filepath.Join(old.Dir, "data", "blob"),
		make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	empty, _ := Create("no-op command")
	empty.MarkDone()

	fresh, _ := Create("small command")
	fresh.MarkDone()
	writeJournal(t, fresh, "create\t/tmp/small\n")

	// budget fits the fresh session but not fresh+old
	if _, err := GC(10, 4096, 0); err != nil {
		t.Fatal(err)
	}
	all, _ := List()
	if len(all) != 1 || all[0].ID != fresh.ID {
		t.Fatalf("gc kept wrong sessions: %+v", all)
	}
}

func TestGCKeepsLiveSessions(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, _ := Create("still running") // live: our own pid, no done marker
	if _, err := GC(10, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Error("gc removed a live session")
	}
}

func TestGCKeepsNewestEvenWhenOversized(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())

	old, _ := Create("older command")
	old.MarkDone()
	writeJournal(t, old, "create\t/tmp/a\n")

	// the newest session alone busts the budget, like deleting one huge
	// file: it is exactly the undo the user is about to reach for
	big, _ := Create("rm -rf huge/")
	big.MarkDone()
	writeJournal(t, big, "create\t/tmp/huge\n")
	if err := os.WriteFile(filepath.Join(big.Dir, "data", "blob"),
		make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(10, 4096, 0); err != nil {
		t.Fatal(err)
	}
	all, _ := List()
	if len(all) != 1 || all[0].ID != big.ID {
		t.Fatalf("newest oversized session should survive, got %+v", all)
	}
}

// Undoing twice in a row targets the newer command first, then the older
// one. A bare redo has to re-apply the older one, because that is the undo
// the user performed last. Picking by command time re-applies the newer
// session and leaves the one they just undid still reverted.
func TestLatestUndoneFollowsUndoOrderNotCommandOrder(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())

	older, err := Create("rm notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	writeJournal(t, older, "unlink\t/tmp/notes.txt\n")

	// session ids are timestamps, so the second create must sort newer
	time.Sleep(10 * time.Millisecond)
	newer, err := Create("rm draft.md")
	if err != nil {
		t.Fatal(err)
	}
	writeJournal(t, newer, "unlink\t/tmp/draft.md\n")

	reload := func(id string) *Session {
		t.Helper()
		all, err := List()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("session %s vanished", id)
		return nil
	}

	// what `undo; undo` does: newest first, then the one before it
	if err := reload(newer.ID).MarkUndone(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := reload(older.ID).MarkUndone(); err != nil {
		t.Fatal(err)
	}

	got, err := LatestUndone()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != older.ID {
		t.Fatalf("redo targeted %q (%s), want the session undone last, %q (%s)",
			got.Cmd, got.ID, reload(older.ID).Cmd, older.ID)
	}

	// after redoing that one, the remaining undone session is the target
	if err := got.ClearUndone(); err != nil {
		t.Fatal(err)
	}
	got, err = LatestUndone()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != newer.ID {
		t.Fatalf("second redo targeted %s, want %s", got.ID, newer.ID)
	}
}

// Markers predating the recorded timestamp are empty files; their mtime has
// to keep working as the undo time so an upgrade does not break redo.
func TestLatestUndoneFallsBackToMarkerMtime(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())

	older, err := Create("rm a")
	if err != nil {
		t.Fatal(err)
	}
	writeJournal(t, older, "unlink\t/tmp/a\n")
	time.Sleep(10 * time.Millisecond)
	newer, err := Create("rm b")
	if err != nil {
		t.Fatal(err)
	}
	writeJournal(t, newer, "unlink\t/tmp/b\n")

	// old format: empty marker, undo order carried only by mtime
	for _, s := range []*Session{newer, older} {
		if err := os.WriteFile(filepath.Join(s.Dir, "undone"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := LatestUndone()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != older.ID {
		t.Fatalf("redo targeted %s, want %s (marked undone last)", got.ID, older.ID)
	}
}

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

func TestRemoveRefusesSymlinkedStoreDirectory(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("something")
	if err != nil {
		t.Fatal(err)
	}
	// A file that has nothing to do with undo.
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A store whose <session-id> component is a symlink pointing at it. On
	// shared storage another user can pre-create this: the shim's mkdir gets
	// EEXIST and carries on, and the lexical shape check still passes.
	vol := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vol, ".undo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(vol, ".undo", s.ID)); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "gone.txt")+"\t"+
		filepath.Join(vol, ".undo", s.ID, "precious.txt")+"\n")

	reloaded, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(precious); err != nil {
		t.Fatal("purge deleted a file outside the store by following a symlinked store directory")
	}
}

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
		{"a rename whose target could not be saved", "rename\t/a\t/b\t-\tlost\n", 1},
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
	// allocatedBytes always includes the session directory itself -- cmd, pid
	// and journal, a couple of hundred bytes -- so this asserts what the 1 MiB
	// backup contributes, not an exact total. Pinning the total would make the
	// test brittle against unrelated session metadata.
	const backup = 1 << 20
	if gotLink >= backup {
		t.Errorf("hardlinked session charged %d bytes; the %d-byte backup must not count",
			gotLink, backup)
	}
	if gotCopy < backup {
		t.Errorf("copied session charged %d bytes, want at least %d", gotCopy, backup)
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

// SweepOrphans deletes directories it finds on disk rather than paths a
// journal named, so its fences are the whole safety argument. This lays out
// everything that must survive it beside the one thing that must not.
func TestSweepOrphansRefusesEverythingItShould(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()
	undo := filepath.Join(vol, ".undo")

	// a live session, so the root gets registered and stays registered
	live, err := Create("rm keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	live.MarkDone()
	lb := filepath.Join(undo, live.ID, "1-1")
	writeBackup(t, lb, 16)
	writeJournal(t, live, "unlink\t"+filepath.Join(vol, "keep.txt")+"\t"+lb+"\tlink\n")

	// (1) the genuine orphan: right shape, ours, no session behind it
	orphan := filepath.Join(undo, "1700000000000001")
	writeBackup(t, filepath.Join(orphan, "1-1"), 16)

	// (2) a name that is not a session id
	notes := filepath.Join(undo, "notes")
	writeBackup(t, filepath.Join(notes, "readme"), 4)

	// (3) numeric but the wrong width -- the shape Create never emits
	shortNum := filepath.Join(undo, "12345")
	writeBackup(t, filepath.Join(shortNum, "x"), 4)

	// (4) a symlink wearing a session-id name, pointing at data we must not touch
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(undo, "1700000000000002")); err != nil {
		t.Fatal(err)
	}

	// (5) a plain file with a session-id name
	stray := filepath.Join(undo, "1700000000000003")
	if err := os.WriteFile(stray, []byte("not a store"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	n, err := SweepOrphans()
	if err != nil {
		t.Fatal(err)
	}

	if n != 1 {
		t.Errorf("swept %d, want exactly 1 (the genuine orphan)", n)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Error("the genuine orphan was not reclaimed")
	}
	for _, keep := range []struct{ what, path string }{
		{"a live session's store", lb},
		{"a non-session name", filepath.Join(notes, "readme")},
		{"a numeric name of the wrong width", filepath.Join(shortNum, "x")},
		{"data behind a symlinked store", precious},
		{"a plain file named like a session", stray},
	} {
		if _, err := os.Lstat(keep.path); err != nil {
			t.Errorf("sweep removed %s (%s)", keep.what, keep.path)
		}
	}
}

// The sweep reads <root>/.undo and deletes what it finds inside. If that
// component is itself a symlink, ReadDir follows it and the fences on the
// entries say nothing about where they actually live.
func TestSweepOrphansRefusesASymlinkedUndoDirectory(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())

	// somewhere else entirely, holding something shaped exactly like a store
	outside := t.TempDir()
	victim := filepath.Join(outside, "1700000000000009")
	writeBackup(t, filepath.Join(victim, "1-1"), 16)

	vol := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(vol, ".undo")); err != nil {
		t.Fatal(err)
	}
	// register the root the way gc does, via a session naming a backup under it
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	s.MarkDone()
	writeJournal(t, s, "unlink\t"+filepath.Join(vol, "x")+"\t"+
		filepath.Join(vol, ".undo", s.ID, "1-1")+"\tlink\n")

	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepOrphans(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatal("the sweep deleted a directory outside the store by following a symlinked .undo")
	}
}

// gc prunes sessions, and a pruned session takes its journal -- the only
// record of where its backups live. If the roots registry could not be
// written, pruning anyway strands any backup Remove then fails to delete.
func TestGCRefusesToPruneWhenTheRegistryCannotBeSaved(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	old, err := Create("rm a")
	if err != nil {
		t.Fatal(err)
	}
	old.MarkDone()
	b := filepath.Join(vol, ".undo", old.ID, "1-1")
	writeBackup(t, b, 16)
	writeJournal(t, old, "unlink\t"+filepath.Join(vol, "a")+"\t"+b+"\tlink\n")

	newer, err := Create("rm b")
	if err != nil {
		t.Fatal(err)
	}
	newer.MarkDone()
	writeJournal(t, newer, "create\t"+filepath.Join(vol, "b")+"\n")

	// a directory where the registry file belongs: WriteFile fails EISDIR
	if err := os.MkdirAll(rootsFile(), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := GC(1, 1, 0); err == nil {
		t.Error("GC reported success although the roots registry could not be written")
	}
	if _, err := os.Stat(old.Dir); err != nil {
		t.Error("GC pruned a session after failing to record where its backups live")
	}
}

func TestCreateRecordsOrigin(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "host"))
	if err != nil {
		t.Fatalf("no host file: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != thisHost() {
		t.Errorf("host file = %q, want %q", got, thisHost())
	}
	got, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != thisHost() {
		t.Errorf("loaded Origin = %q, want %q", got.Origin, thisHost())
	}
}

// A session written by an older hook has no host file. It must load cleanly
// with an empty Origin, which Live treats as "use the pid probe, as before".
func TestLoadWithoutOriginIsEmpty(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Dir, "host")); err != nil {
		t.Fatal(err)
	}
	got, err := Get(s.ID)
	if err != nil {
		t.Fatalf("a session without a host file must still load: %v", err)
	}
	if got.Origin != "" {
		t.Errorf("Origin = %q, want empty", got.Origin)
	}
}

// thisHost must be stable within a process: Live compares against it on every
// call, and a value that varied would make liveness flap.
//
// Deliberately does not assert non-empty. The contract permits an empty result
// -- a host where neither the hostname nor the boot id can be read -- and Live
// resolves that case explicitly. Asserting otherwise would make this test fail
// on a restricted container even where the fallback is working correctly.
func TestThisHostIsStableAndOneLine(t *testing.T) {
	a, b := thisHost(), thisHost()
	if a != b {
		t.Errorf("thisHost not stable: %q then %q", a, b)
	}
	if strings.Contains(a, "\n") {
		t.Errorf("thisHost must be one line, got %q", a)
	}
}

// A partial identity is worse than none: it matches sessions it should not.
func TestComposeHostRequiresEveryPart(t *testing.T) {
	if got := composeHost("node1", "", "pid:[1]"); got != "" {
		t.Errorf("no boot id = %q, want empty: a session from before a reboot "+
			"would look local and its pid may since have been reissued", got)
	}
	if got := composeHost("", "boot-uuid", "pid:[1]"); got != "" {
		t.Errorf("no hostname = %q, want empty", got)
	}
	if got := composeHost("node1", "boot-uuid", ""); got != "" {
		t.Errorf("no pid namespace = %q, want empty: containers sharing a "+
			"kernel and a UTS namespace would look identical while the same "+
			"pid names unrelated processes in each", got)
	}
	want := "node1\tboot-uuid\tpid:[1]"
	if got := composeHost("node1", "boot-uuid", "pid:[1]"); got != want {
		t.Errorf("composeHost = %q, want %q", got, want)
	}
}

// The pid namespace is what makes this identity mean "where this pid is
// meaningful" rather than merely "which machine".
func TestThisHostCarriesThePidNamespace(t *testing.T) {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Skipf("pid namespace unreadable: %v", err)
	}
	if !strings.Contains(thisHost(), strings.TrimSpace(ns)) {
		t.Errorf("thisHost %q does not carry the pid namespace %q", thisHost(), ns)
	}
}

// The shell hooks and Create must agree on what identifies this host, or a
// session created by the fish hook reads as foreign to the Go binary running
// on the same machine -- pinned for the whole grace, and undo refusing to
// revert it. `uname -n` is gethostname(2) by POSIX definition, which is what
// bash's $HOSTNAME, zsh's $HOST and Go's os.Hostname all report. `hostname`
// the command is not: it may print the FQDN where gethostname returns the
// short name.
func TestUnameMatchesGoHostname(t *testing.T) {
	out, err := exec.Command("uname", "-n").Output()
	if err != nil {
		t.Skipf("uname unavailable: %v", err)
	}
	name, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != name {
		t.Errorf("uname -n = %q but os.Hostname = %q; the hooks and the binary "+
			"would disagree about which sessions are local", got, name)
	}
}

// foreignSession builds a session that looks like one written by another node:
// an origin that is not ours, a pid that does not resolve here, no done marker.
func foreignSession(t *testing.T, age time.Duration) *Session {
	t.Helper()
	start := time.Now().Add(-age)
	id := fmt.Sprintf("%d%06d", start.Unix(), start.Nanosecond()/1000)
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	// a pid that is not running here; above every pid_max
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"),
		[]byte("othernode\tnot-our-boot-id\tpid:[999]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLiveTreatsAForeignRunningSessionAsLive(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Hour)
	if !s.Live() {
		t.Error("a session from another node with no done marker was read as finished; " +
			"gc would delete the backups of a command that is still running")
	}
}

// A foreign session whose pid file has not been written yet -- or was written
// short, or cannot be parsed -- loads with Pid 0. That must not be read as
// "finished": on a shared store it is the ordinary look of a session whose
// creator is still setting it up.
func TestLiveDoesNotConsultThePidOfAForeignSession(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.Remove(filepath.Join(s.Dir, "pid")); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pid != 0 {
		t.Fatalf("setup: want Pid 0, got %d", s.Pid)
	}
	if !s.Live() {
		t.Error("a foreign session with no readable pid was called finished")
	}
}

// The grace is what keeps an abandoned foreign session from being pinned
// forever: past it, the session is collectible again.
func TestLiveExpiresAForeignSessionPastTheGrace(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	t.Setenv("UNDO_FOREIGN_GRACE", "3600")
	s := foreignSession(t, 2*time.Hour)
	if s.Live() {
		t.Error("a foreign session older than the grace must be collectible")
	}
}

// withLocalHost forces thisHost's answer for one test. hostOnce is marked done
// so the real lookup cannot overwrite it afterwards.
func withLocalHost(t *testing.T, id string) {
	t.Helper()
	hostOnce.Do(func() {})
	prev := hostID
	hostID = id
	t.Cleanup(func() { hostID = prev })
}

// With no local identity nothing can be classified, so neither signal is sound
// alone: a session is finished only when the probe and the grace agree.
// Otherwise a local command that outlived the grace could be purged, or an
// undo applied over it, while it was still writing.
func TestLiveWithNoLocalIdentityNeedsBothSignals(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	withLocalHost(t, "")

	// far past any grace, but its pid is ours and demonstrably running
	s := foreignSession(t, 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("a running pid must still count as live when the local identity " +
			"is unknown; purge and apply would act on a running command")
	}
}

// The grace cannot be tuned below the floor: a grace shorter than the clock
// skew between two nodes would collect a command that just started.
func TestForeignGraceHasAFloor(t *testing.T) {
	t.Setenv("UNDO_FOREIGN_GRACE", "1")
	if got := foreignGrace(); got != minForeignGrace {
		t.Errorf("grace = %v, want the floor %v", got, minForeignGrace)
	}
}

// A grace too large for a time.Duration must saturate rather than wrap. The
// wrapped value is negative, falls under the floor, and silently becomes 15
// minutes -- the exact opposite of what setting an enormous number asks for,
// and it would collect running commands after a quarter of an hour.
func TestForeignGraceSaturatesInsteadOfWrapping(t *testing.T) {
	t.Setenv("UNDO_FOREIGN_GRACE", "10000000000") // ~317 years, overflows
	got := foreignGrace()
	if got <= 0 {
		t.Fatalf("grace = %v, which wrapped negative", got)
	}
	if got < 100*365*24*time.Hour {
		t.Errorf("grace = %v; an enormous setting collapsed instead of saturating", got)
	}
}

// A foreign session that finished normally left a done marker, and that is
// conclusive from any node -- no grace needed.
func TestLiveHonoursDoneOnAForeignSession(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.WriteFile(filepath.Join(s.Dir, "done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("a foreign session with a done marker is finished")
	}
}

// Sessions written before this change have no origin. They must keep the old
// behaviour exactly, or a rollout strands every session already on disk.
func TestLiveFallsBackToTheProbeWithoutAnOrigin(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.Remove(filepath.Join(s.Dir, "host")); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("without an origin the pid probe decides, and this pid is not running")
	}
}

// Same host: the probe is meaningful and must still be what decides.
func TestLiveProbesThePidOnItsOwnHost(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.WriteFile(filepath.Join(s.Dir, "host"),
		[]byte(thisHost()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("our own running pid on our own host must read as live")
	}
}

// The reproduced defect, at the level it actually bit: ordinary retention, no
// budget pressure, and a command running on another node loses its session and
// its backups mid-flight.
func TestGCKeepsASessionRunningOnAnotherNode(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	running := foreignSession(t, time.Hour)
	bak := filepath.Join(vol, ".undo", running.ID, "1360-1")
	writeBackup(t, bak, 64)
	writeJournal(t, running,
		"unlink\t"+filepath.Join(vol, "gone.txt")+"\t"+bak+"\tlink\n")

	// the user's login shell meanwhile: more finished commands than keep
	for i := 0; i < 5; i++ {
		s := mustSession(t, fmt.Sprintf("rm filler-%d", i))
		writeJournal(t, s, "unlink\t/x\t-\tnone\n")
	}

	if _, err := GC(2, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(running.Dir); err != nil {
		t.Error("gc deleted the session of a command still running on another node")
	}
	if _, err := os.Stat(bak); err != nil {
		t.Error("gc deleted the backup of a command still running on another node")
	}
}

// mustSession creates a done session. Create's own collision loop guarantees
// ids are distinct and ordered, so no sleep is needed between calls.
func mustSession(t *testing.T, cmd string) *Session {
	t.Helper()
	s, err := Create(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(); err != nil {
		t.Fatal(err)
	}
	return s
}

// A session that has been created but has not yet recorded anything, and has
// not been marked done, may be moments from its first entry -- or may be one
// whose creator is on another node and still writing it out. Deleting it takes
// the directory the command is about to journal into.
func TestGCSparesAYoungSessionThatHasRecordedNothingYet(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	starting, err := Create("a command that has not touched a file yet")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(starting.Dir, "pid")); err != nil {
		t.Fatal(err) // as a reader sees it before the pid is written
	}
	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(starting.Dir); err != nil {
		t.Error("gc deleted a session that was still being set up")
	}
}
