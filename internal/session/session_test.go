package session

import (
	"os"
	"path/filepath"
	"testing"
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
