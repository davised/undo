package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edaywalid/undo/internal/journal"
)

// The probe must answer for a real directory without reporting an error.
// Whether the answer is true or false depends on the filesystem under the
// test's temp dir, so the assertion is on the error, not on the verdict.
func TestProbeReflinkAnswersWithoutError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe only reports a real answer on linux")
	}
	if _, err := probeReflink(t.TempDir()); err != nil {
		t.Fatalf("probeReflink returned an error on a writable dir: %v", err)
	}
}

// A directory that does not exist is a probe failure, not "unsupported":
// answering false there would report a filesystem property we never measured.
func TestProbeReflinkErrorsOnMissingDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the !linux twin reports no error for any directory")
	}
	if _, err := probeReflink("/nonexistent-undo-doctor-dir"); err == nil {
		t.Fatal("probeReflink reported no error for a directory that does not exist")
	}
}

// The probe leaves nothing behind: doctor runs it in the user's own directory.
func TestProbeReflinkCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe only creates files on linux")
	}
	dir := t.TempDir()
	if _, err := probeReflink(dir); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("probe left %d file(s) behind", len(ents))
	}
}

// A reported success must have actually moved the data. This holds on any
// filesystem: where cloning is unsupported the probe reports false and the
// assertion does not apply, and where it is supported the destination must
// contain what the source did. It is what separates the real ioctl from a
// function that returns true without calling it.
func TestTryCloneThatSucceedsMovedTheData(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the clone only happens on linux")
	}
	dir := t.TempDir()
	const body = "undo doctor clone payload\n"

	src, err := os.CreateTemp(dir, "src-")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.WriteString(body); err != nil {
		t.Fatal(err)
	}

	dst, err := os.CreateTemp(dir, "dst-")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	ok, err := tryClone(dst, src)
	if err != nil {
		t.Fatalf("tryClone: %v", err)
	}
	if !ok {
		t.Skip("this filesystem cannot clone; nothing to verify")
	}
	got, err := os.ReadFile(dst.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("clone reported success but dst holds %q, want %q", got, body)
	}
}

func TestStoreRootOfTakesTheComponentBeforeUndo(t *testing.T) {
	root, ok := storeRootOf("/data/me/.undo/1785717763234605/4882-1", "1785717763234605")
	if !ok || root != "/data/me" {
		t.Fatalf("got %q ok=%v, want /data/me true", root, ok)
	}
}

// A path shaped like a store but belonging to some other session is not this
// session's store, and reporting it as one would name a directory this run
// never wrote to.
func TestStoreRootOfRejectsAnotherSessionsStore(t *testing.T) {
	if _, ok := storeRootOf("/data/me/.undo/1111111111111111/x-1", "2222222222222222"); ok {
		t.Fatal("accepted a store belonging to a different session")
	}
}

func TestStoreRootOfRejectsAnEmptyPrefix(t *testing.T) {
	if _, ok := storeRootOf("/.undo/1785717763234605/x-1", "1785717763234605"); ok {
		t.Fatal("accepted the filesystem root as a store root")
	}
}

func TestClassifyReadsTheMethodOfTheMatchingUnlink(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpStoreMove, Fields: []string{"/data/me/.undo", "-"}},
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/data/me/.undo/99/1", "link"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if v.Problem != "" {
		t.Fatalf("unexpected problem: %s", v.Problem)
	}
	if v.Method != "link" || v.StoreRoot != "/data/me" || v.Fallback || v.Lost {
		t.Fatalf("got %+v", v)
	}
}

// A backup that failed is written as `lost <victim> unlink`, NOT as an unlink
// record. Matching only OpUnlink goes blind exactly when nothing was saved,
// which is the case most worth reporting.
func TestClassifyMatchesTheLostRecordForAFailedBackup(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpLost, Fields: []string{"/t/canary", "unlink"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if v.Problem != "" {
		t.Fatalf("unexpected problem: %s", v.Problem)
	}
	if !v.Lost {
		t.Fatalf("a lost record was not reported as lost: %+v", v)
	}
}

func TestClassifyReportsTheSessionStoreFallback(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/store/sessions/99/data/1", "copy"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if !v.Fallback || v.StoreRoot != "" {
		t.Fatalf("got %+v, want the fallback", v)
	}
}

// journal.Read keeps a corrupt record's Op and drops its fields, so this is
// what a corrupt canary record really looks like by the time classify sees it.
// It cannot match the victim, which is why the no-match path has to look for it.
func TestClassifyReportsCorruptionRatherThanSilence(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Corrupt: true},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if v.Problem == "" {
		t.Fatal("a corrupt journal was not reported at all")
	}
	if !strings.Contains(v.Problem, "integrity") {
		t.Fatalf("problem = %q, want it to name the integrity failure rather "+
			"than blaming the shim for recording nothing", v.Problem)
	}
}

// A corrupt record alongside the real one must not be counted as a second
// match: it has no fields to match with.
func TestClassifyIgnoresACorruptRecordBesideAGoodOne(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Corrupt: true},
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/1", "link"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if v.Problem != "" {
		t.Fatalf("unexpected problem: %s", v.Problem)
	}
	if v.StoreRoot != "/d" || v.Method != "link" {
		t.Fatalf("got %+v, want the good record classified", v)
	}
}

// The marker can appear more than once; the valid one is the component
// followed by this run's session id.
func TestStoreRootOfHandlesARepeatedMarker(t *testing.T) {
	root, ok := storeRootOf("/a/.undo/x/.undo/99/f", "99")
	if !ok || root != "/a/.undo/x" {
		t.Fatalf("got %q ok=%v, want /a/.undo/x true", root, ok)
	}
}

// "-" is a discarded backup: nothing was saved.
func TestClassifyTreatsADiscardedBackupAsLost(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "-", "none"}},
	}
	if v := classify(entries, "/t/canary", "/store/sessions/99", "99"); !v.Lost {
		t.Fatalf("got %+v, want lost", v)
	}
}

// A method of "none" is how the reader reports a record whose save did not
// happen, so it means the same as a missing backup path even when a path is
// present: deciding loss from the path alone would report a volume as healthy,
// store root and all, while its own journal says nothing was saved.
func TestClassifyTreatsAMethodOfNoneAsLost(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/1", "none"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if !v.Lost {
		t.Fatalf("got %+v, want lost: the journal says no save method succeeded", v)
	}
}

// A sibling directory whose name merely starts with the session dir is not
// the session dir.
func TestClassifyDoesNotMistakeASiblingForTheSessionStore(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/store/sessions/999/x/1", "copy"}},
	}
	v := classify(entries, "/t/canary", "/store/sessions/99", "99")
	if v.Fallback {
		t.Fatalf("got %+v, want no fallback: sibling named like the session dir", v)
	}
	if v.Problem == "" {
		t.Fatalf("got %+v, want a problem classifying the sibling's backup", v)
	}
}

// One deletion producing two records is a defect to surface, not something to
// resolve by silently taking the first or the last.
func TestClassifyRefusesTwoMatches(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/1", "link"}},
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/2", "link"}},
	}
	if v := classify(entries, "/t/canary", "/store/sessions/99", "99"); v.Problem == "" {
		t.Fatal("accepted two records for one deletion")
	}
}

func TestClassifyReportsNothingRecorded(t *testing.T) {
	if v := classify(nil, "/t/canary", "/store/sessions/99", "99"); v.Problem == "" {
		t.Fatal("an empty journal was not reported as a problem")
	}
}

// An unrecognised token is printed rather than mapped, so that a future save
// method does not read as a failure.
func TestClassifyPassesAnUnknownMethodThrough(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/1", "reflink"}},
	}
	if v := classify(entries, "/t/canary", "/store/sessions/99", "99"); v.Method != "reflink" {
		t.Fatalf("method = %q, want it passed through verbatim", v.Method)
	}
}

func TestCheckVolumeRejectsAMissingTarget(t *testing.T) {
	if _, err := checkVolume("", "/nonexistent-undo-doctor-target"); err == nil {
		t.Fatal("a missing target did not produce an error")
	}
}

// The canary must never be created inside a directory doctor made: the
// resolver takes the HIGHEST owned, writable ancestor, so a doctor-owned
// subdirectory becomes the store root on exactly the volumes where a real file
// would find none -- reporting a healthy store where there is not one.
// Whatever happens, nothing is left in the user's directory.
func TestCheckVolumeLeavesNothingBehind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs the shim")
	}
	dir := t.TempDir()
	_, _ = checkVolume("", dir) // no shim: the round trip fails, cleanup must not
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("checkVolume left %d entries behind", len(ents))
	}
}

// checkVolume must not leave a session in the store, whatever the round trip
// concluded. This pins that and only that: with no shim there is no
// distributed backup, so it cannot and does not exercise the reload in the
// deferred cleanup. Proving the reload needs a real store on a target
// filesystem, which needs the shim, so that assertion lives in the e2e suite.
func TestCheckVolumeLeavesNoSessionBehind(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs the session store")
	}
	store := t.TempDir()
	t.Setenv("UNDO_DATA_DIR", store)

	before, err := os.ReadDir(filepath.Join(store, "sessions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	// no shim, so the round trip reports a problem rather than succeeding
	if _, err := checkVolume("", t.TempDir()); err != nil {
		t.Fatalf("checkVolume returned a target error: %v", err)
	}
	after, err := os.ReadDir(filepath.Join(store, "sessions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("checkVolume left %d session(s) behind, was %d",
			len(after), len(before))
	}
}
