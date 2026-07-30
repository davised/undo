package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDecodesEscapes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "journal")
	content := "unlink\t/tmp/a%09b%0Ac%25d\t/bak/1\ncreate\t/tmp/x\nbogus\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (bogus line skipped)", len(entries))
	}
	if entries[0].Op != OpUnlink {
		t.Errorf("op = %q", entries[0].Op)
	}
	want := "/tmp/a\tb\nc%d"
	if entries[0].Fields[0] != want {
		t.Errorf("decoded path = %q, want %q", entries[0].Fields[0], want)
	}
	if entries[1].Op != OpCreate || entries[1].Fields[0] != "/tmp/x" {
		t.Errorf("second entry = %+v", entries[1])
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "nope"))
	if !os.IsNotExist(err) {
		t.Errorf("want not-exist error, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Op: OpUnlink, Fields: []string{"/a", "/b"}}, "deleted   /a"},
		{Entry{Op: OpRename, Fields: []string{"/a", "/b", "-"}}, "moved     /a -> /b"},
		{Entry{Op: OpChmod, Fields: []string{"/a", "644", "600"}}, "mode      /a (644 -> 600)"},
	}
	for _, c := range cases {
		if got := c.e.Describe(); got != c.want {
			t.Errorf("Describe(%v) = %q, want %q", c.e, got, c.want)
		}
	}
}

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
