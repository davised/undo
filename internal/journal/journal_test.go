package journal

import (
	"fmt"
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
