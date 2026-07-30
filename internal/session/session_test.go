package session

import (
	"os"
	"path/filepath"
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
	if _, err := GC(10, 4096); err != nil {
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
	if _, err := GC(10, 1<<30); err != nil {
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

	if _, err := GC(10, 4096); err != nil {
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
