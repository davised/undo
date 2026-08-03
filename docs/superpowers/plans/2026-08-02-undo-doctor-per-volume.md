# Per-volume `undo doctor` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `undo doctor` answer "is my work protected *here*" for the current directory and for any directories named on the command line, instead of reporting on whatever `$TMPDIR` happens to name.

**Architecture:** The live capture/restore moves out of `$TMPDIR` and into the target directory, and the store root is read back out of the journal the shim just wrote rather than predicted by a second copy of the resolver. A canary file is created *directly* in the canonicalized target, deleted through the armed shim, classified from its journal record, restored, verified, and removed. `$TMPDIR` becomes a control that runs only when a target fails at capture or restore.

**Tech Stack:** Go 1.24 (stdlib only — `go.mod` requires nothing and must keep requiring nothing), bash (e2e suites), podman/docker via `test/in-container.sh`.

## Global Constraints

Copied from `docs/design/undo-doctor-per-volume-design.md` and `AGENTS.md`. Every task inherits these.

- **The tool is Linux-only; the workstation is macOS.** Never run `make`, `go test`, `gcc`, or `test/e2e.sh` directly. Everything goes through `test/in-container.sh`. `test/multifs.sh` additionally needs `--privileged`.
- **No new module dependencies.** `go.mod` requires nothing. Do not add `golang.org/x/sys`.
- **The package must build and vet on macOS**, so every Linux-only symbol needs a `!linux` twin.
- **No site-specific data** anywhere. `tools/check-no-site-data.sh` enforces it; `.check-no-site-data-ignore` is empty and stays empty.
- **No shim change in this plan.** The glibc floor is untouched, and `CONTRIBUTING.md`'s "an e2e case per shim change" does not apply — cases are added anyway.
- **`test/e2e.sh` case 23 greps the literal strings `[ok  ] capture` and `[ok  ] restore`.** Those labels stay.
- **The suite's success banner is the last line of `test/e2e.sh`.** Anything appended goes above it.
- Verify with `test/in-container.sh make test`, and `tools/check-no-site-data.sh` on every file touched.

## File Structure

| File | Responsibility |
| --- | --- |
| `cmd/undo/reflink_linux.go` | **create** — the `FICLONE` ioctl probe |
| `cmd/undo/reflink_other.go` | **create** — the non-Linux twin |
| `cmd/undo/volume.go` | **create** — canonicalize, canary, classify, one volume's verdict |
| `cmd/undo/volume_test.go` | **create** — unit tests for the pure classification helpers |
| `cmd/undo/doctor.go` | modify — `cmdDoctor([]string)`, control policy, output, two pre-existing defects |
| `cmd/undo/main.go` | modify — pass `args[1:]` to `cmdDoctor`, usage line |
| `test/e2e.sh` | modify — new cases, above the banner |
| `test/multifs-doctor.sh` | **create** — assertions run through the multifs harness |
| `README.md` | modify — document `undo doctor [path...]` |

---

### Task 1: The reflink probe

Self-contained and testable on its own: no doctor wiring yet.

**Files:**
- Create: `cmd/undo/reflink_linux.go`
- Create: `cmd/undo/reflink_other.go`
- Test: `cmd/undo/volume_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func probeReflink(dir string) (supported bool, err error)` — available on every platform. Returns `(false, nil)` when the filesystem cannot clone, `(true, nil)` when it can, and a non-nil error only when the probe itself failed.

- [ ] **Step 1: Write the failing test**

Add to `cmd/undo/volume_test.go`:

```go
package main

import (
	"runtime"
	"testing"
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
```

Add `"os"` to that file's imports.

- [ ] **Step 2: Run the test and watch it fail**

```bash
test/in-container.sh go test ./cmd/undo/ -run TestProbeReflink -v
```

Expected: a build failure, `undefined: probeReflink`.

- [ ] **Step 3: Write the Linux probe**

Create `cmd/undo/reflink_linux.go`:

```go
//go:build linux

package main

import (
	"os"
	"syscall"
)

// ficlone is _IOW(0x94, 9, int): ask the filesystem to share extents between
// two files rather than copy them. The number is written out because importing
// golang.org/x/sys for it would be this project's first module dependency.
const ficlone = 0x40049409

// probeReflink reports whether dir's filesystem can clone extents.
//
// FICLONE is the honest probe -- unlike copy_file_range it either shares
// extents or fails, so its result distinguishes a free clone from an ordinary
// copy. Both files are created in dir, so both are on the filesystem being
// asked about.
func probeReflink(dir string) (bool, error) {
	src, err := os.CreateTemp(dir, ".undo-doctor-clone-src-")
	if err != nil {
		return false, err
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.Write([]byte("undo doctor reflink probe\n")); err != nil {
		return false, err
	}

	dst, err := os.CreateTemp(dir, ".undo-doctor-clone-dst-")
	if err != nil {
		return false, err
	}
	defer os.Remove(dst.Name())
	defer dst.Close()

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	switch errno {
	case 0:
		return true, nil
	case syscall.EOPNOTSUPP, syscall.ENOTTY, syscall.EINVAL:
		// the filesystem cannot do it: a real answer, not a broken probe
		return false, nil
	case syscall.EXDEV:
		// both files were created in one directory, so this cannot happen
		// unless the probe itself is wrong. Do not report it as unsupported.
		return false, fmt.Errorf("reflink probe crossed a filesystem boundary inside %s", dir)
	default:
		return false, errno
	}
}
```

Add `"fmt"` to the imports.

- [ ] **Step 4: Write the non-Linux twin**

Create `cmd/undo/reflink_other.go`:

```go
//go:build !linux

package main

// probeReflink has no answer off Linux. undo does not run there, but gofmt and
// go vet do, on the macOS workstation these agents work from, and a build tag
// on the real probe alone leaves the call site undefined.
func probeReflink(dir string) (bool, error) {
	return false, nil
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
test/in-container.sh go test ./cmd/undo/ -run TestProbeReflink -v
```

Expected: PASS (three tests).

- [ ] **Step 6: Commit**

```bash
git add cmd/undo/reflink_linux.go cmd/undo/reflink_other.go cmd/undo/volume_test.go
git commit -m "doctor: probe whether a filesystem can clone extents"
```

---

### Task 2: Classifying a journal record

Pure functions, no filesystem, no shim. This is where every rule the review pass found lives, so it is worth its own test cycle.

**Files:**
- Create: `cmd/undo/volume.go`
- Test: `cmd/undo/volume_test.go`

**Interfaces:**
- Consumes: `probeReflink` from Task 1 (not called yet).
- Produces:
  - `type volumeVerdict struct { StoreRoot string; Fallback bool; Method string; Lost bool; Problem string }`
  - `func storeRootOf(backup, sessionID string) (string, bool)`
  - `func classify(entries []journal.Entry, victim, sessionDir, sessionID string) volumeVerdict`

  `Problem` is empty when the record was classified; when set it holds the reason the volume could not be judged, and the caller reports FAIL. `Lost` means nothing was saved. `Fallback` means the backup landed in the session store rather than a filesystem-local one.

- [ ] **Step 1: Write the failing tests**

Add to `cmd/undo/volume_test.go`:

```go
import "github.com/edaywalid/undo/internal/journal"

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

func TestClassifyRefusesACorruptRecord(t *testing.T) {
	entries := []journal.Entry{
		{Op: journal.OpUnlink, Fields: []string{"/t/canary", "/d/.undo/99/1", "link"}, Corrupt: true},
	}
	if v := classify(entries, "/t/canary", "/store/sessions/99", "99"); v.Problem == "" {
		t.Fatal("classified from a record that failed its integrity check")
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
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
test/in-container.sh go test ./cmd/undo/ -run 'TestStoreRootOf|TestClassify' -v
```

Expected: build failure, `undefined: storeRootOf`, `undefined: classify`.

- [ ] **Step 3: Write the implementation**

Create `cmd/undo/volume.go`:

```go
package main

import (
	"fmt"
	"strings"

	"github.com/edaywalid/undo/internal/journal"
)

// volumeVerdict is what one target directory's round trip established.
//
// Problem is empty when the record was classified. When set, it is the reason
// the volume could not be judged, and the caller reports FAIL rather than
// guessing at a healthy answer.
type volumeVerdict struct {
	StoreRoot string // "" when the backup did not land in a filesystem-local store
	Fallback  bool   // the backup went to the session store instead
	Method    string // link, copy, none, or an unrecognised token verbatim
	Lost      bool   // nothing was saved for the canary
	Problem   string
}

// storeRootOf splits a backup path of the form <root>/.undo/<session-id>/<name>
// and returns <root>.
//
// The session id must be this run's own. A merely session-id-shaped component
// would accept an unrelated .undo directory elsewhere in the tree and report it
// as the store this run wrote to.
func storeRootOf(backup, sessionID string) (string, bool) {
	const marker = "/.undo/"
	for i := 0; ; {
		j := strings.Index(backup[i:], marker)
		if j < 0 {
			return "", false
		}
		at := i + j
		root := backup[:at]
		rest := backup[at+len(marker):]
		if slash := strings.Index(rest, "/"); root != "" && slash > 0 && rest[:slash] == sessionID {
			return root, true
		}
		i = at + len(marker)
	}
}

// matches reports whether e is the record for victim's deletion. Two ops
// qualify: a successful unlink, and the lost record written in its place when
// the backup could not be taken.
func matches(e journal.Entry, victim string) bool {
	switch e.Op {
	case journal.OpUnlink:
		return len(e.Fields) > 0 && e.Fields[0] == victim
	case journal.OpLost:
		return len(e.Fields) > 1 && e.Fields[0] == victim && e.Fields[1] == journal.OpUnlink
	}
	return false
}

// classify turns the journal the shim just wrote into one volume's verdict.
//
// Selection is by op and exact path, never by position: journals legitimately
// carry other records -- storemv among them -- and records that failed their
// integrity check keep their slots rather than being filtered out.
func classify(entries []journal.Entry, victim, sessionDir, sessionID string) volumeVerdict {
	var found []journal.Entry
	for _, e := range entries {
		if matches(e, victim) {
			found = append(found, e)
		}
	}
	switch {
	case len(found) == 0:
		return volumeVerdict{Problem: "the shim recorded no deletion of the canary"}
	case len(found) > 1:
		return volumeVerdict{Problem: fmt.Sprintf(
			"%d records for one deletion; the journal disagrees with itself", len(found))}
	}
	e := found[0]
	if e.Corrupt {
		return volumeVerdict{Problem: "the canary's journal record failed its integrity check"}
	}
	if e.Op == journal.OpLost {
		return volumeVerdict{Lost: true, Method: "none"}
	}

	v := volumeVerdict{Method: e.Method()}
	backup := e.Backup()
	switch {
	case backup == "" || backup == "-":
		v.Lost = true
	case strings.HasPrefix(backup, sessionDir+"/"):
		v.Fallback = true
	default:
		root, ok := storeRootOf(backup, sessionID)
		if !ok {
			return volumeVerdict{Problem: "the backup landed somewhere unexpected: " + backup}
		}
		v.StoreRoot = root
	}
	return v
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
test/in-container.sh go test ./cmd/undo/ -run 'TestStoreRootOf|TestClassify' -v
```

Expected: PASS (nine tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/undo/volume.go cmd/undo/volume_test.go
git commit -m "doctor: classify a canary's journal record"
```

---

### Task 3: One volume's round trip

**Files:**
- Modify: `cmd/undo/volume.go`
- Modify: `cmd/undo/doctor.go` (extract the round trip so both callers share it)

**Interfaces:**
- Consumes: `classify`, `probeReflink`, `armedEnv(base []string, shim, dir string) []string` from `run.go`, `session.Create`, `session.Get`, `restore.Run`.
- Produces: `func checkVolume(shim, target string) (volumeVerdict, error)` — a non-nil error is a target-level failure (missing, unresolvable, unwritable) that never reached the shim, and therefore one the control cannot say anything about. Everything the shim *did* reach is reported through `volumeVerdict.Problem` instead.

- [ ] **Step 1: Write the failing test**

Add to `cmd/undo/volume_test.go`:

```go
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
```

- [ ] **Step 2: Run the test and watch it fail**

```bash
test/in-container.sh go test ./cmd/undo/ -run TestCheckVolume -v
```

Expected: build failure, `undefined: checkVolume`.

- [ ] **Step 3: Implement `checkVolume`**

Append to `cmd/undo/volume.go`:

```go
// checkVolume runs one capture/restore round trip inside target and reports
// what it established about that filesystem.
//
// The canary is a FILE created directly in target, never inside a directory
// this function creates. resolve_store_root takes the highest ancestor that is
// on the same device, owned by the caller and writable; a directory doctor just
// made is owned by the caller by construction, so putting the canary inside one
// would supply a qualifying ancestor on exactly the volumes where a real file
// finds none -- and doctor would report a healthy local store for a directory
// whose real files fall back to capped copies.
func checkVolume(shim, target string) (volumeVerdict, error) {
	dir, err := filepath.EvalSymlinks(target)
	if err != nil {
		return volumeVerdict{}, err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return volumeVerdict{}, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return volumeVerdict{}, err
	}
	if !fi.IsDir() {
		return volumeVerdict{}, fmt.Errorf("%s is not a directory", target)
	}

	f, err := os.CreateTemp(dir, ".undo-doctor-")
	if err != nil {
		return volumeVerdict{}, err
	}
	victim := f.Name()
	const body = "undo doctor canary\n"
	_, werr := f.WriteString(body)
	f.Close()
	// Cleanup runs unarmed: os.Remove is a raw syscall from Go, which
	// LD_PRELOAD never sees, so it cannot journal an operation or mint a
	// session of its own as a side effect of a diagnostic.
	defer os.Remove(victim)
	if werr != nil {
		return volumeVerdict{}, werr
	}

	sess, err := session.Create("undo doctor volume check")
	if err != nil {
		return volumeVerdict{}, err
	}
	defer sess.Remove() // classification happens first: Remove deletes the very backups being classified

	// The victim is passed as an argument, never concatenated into the script:
	// once a user-supplied path reaches here, a space or a metacharacter would
	// otherwise change what runs.
	cmd := exec.Command("/bin/sh", "-c", `rm -- "$1"`, "sh", victim)
	cmd.Env = armedEnv(os.Environ(), shim, sess.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return volumeVerdict{}, fmt.Errorf("rm failed: %v %s", err, out)
	}
	sess.MarkDone()

	entries, err := journal.Read(filepath.Join(sess.Dir, "journal"))
	if err != nil && !os.IsNotExist(err) {
		// Discarding this would surface a truncated journal as "nothing was
		// recorded", which is a different and far more alarming diagnosis.
		return volumeVerdict{Problem: "the journal could not be read: " + err.Error()}, nil
	}
	v := classify(entries, victim, sess.Dir, sess.ID)

	if v.Problem == "" && !v.Lost {
		fresh, err := session.Get(sess.ID)
		if err != nil {
			v.Problem = "the session could not be reloaded: " + err.Error()
		} else if _, err := restore.Run(fresh, restore.Undo, restore.Options{}); err != nil {
			v.Problem = "restore failed: " + err.Error()
		} else if got, err := os.ReadFile(victim); err != nil || string(got) != body {
			v.Problem = "the canary came back with different contents"
		}
	}

	if supported, err := probeReflink(dir); err == nil {
		v.Reflink = supported
		v.ReflinkKnown = true
	}
	return v, nil
}
```

Add to the `volumeVerdict` struct:

```go
	Reflink      bool // the target's filesystem can clone extents
	ReflinkKnown bool // the probe produced an answer at all
```

Add these imports to `volume.go`: `"os"`, `"os/exec"`, `"path/filepath"`, and the `internal/restore` and `internal/session` packages.

- [ ] **Step 4: Run the tests and watch them pass**

```bash
test/in-container.sh go test ./cmd/undo/ -run TestCheckVolume -v
```

Expected: PASS (two tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/undo/volume.go cmd/undo/volume_test.go
git commit -m "doctor: run the round trip in the target directory"
```

---

### Task 4: The command surface

**Files:**
- Modify: `cmd/undo/doctor.go`
- Modify: `cmd/undo/main.go:459` and the usage block at `cmd/undo/main.go:35`

**Interfaces:**
- Consumes: `checkVolume` from Task 3.
- Produces: `cmdDoctor(targets []string)`.

- [ ] **Step 1: Change the signature and the dispatch**

In `cmd/undo/main.go`, replace `cmdDoctor()` with `cmdDoctor(args[1:])`, and change the usage line to:

```
  undo doctor [path...]   check the install, and whether files are protected here
```

In `cmd/undo/doctor.go`, change `func cmdDoctor() {` to `func cmdDoctor(targets []string) {`, and immediately after the `report` closure:

```go
	if len(targets) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			report(failed, "volume", "cannot determine the working directory: "+err.Error())
			wd = ""
		}
		if wd != "" {
			targets = []string{wd}
		}
	}
```

- [ ] **Step 2: Replace the `$TMPDIR` round trip with per-target checks**

Replace the `if shim != "" { reportRoundTrip(report, shim) }` block with:

```go
	if shim != "" {
		controlRun, controlOK := false, false
		for _, t := range targets {
			v, err := checkVolume(shim, t)
			if err != nil {
				// never reached the shim, so the control would say nothing
				report(failed, "volume "+t, err.Error())
				continue
			}
			// The literal labels below are asserted by test/e2e.sh case 23.
			if v.Problem != "" {
				if !controlRun {
					controlRun, controlOK = true, controlPasses(shim)
				}
				if controlOK {
					report(failed, "capture", v.Problem+
						"; the same check passes in the temporary directory, so this volume is the difference")
				} else {
					report(failed, "capture", v.Problem+
						"; it also fails in the temporary directory, so this volume is not implicated")
				}
				continue
			}
			report(pass, "capture", "1 change recorded")
			report(pass, "restore", "canary recovered intact")
			reportVolume(report, t, v)
		}
	}
```

- [ ] **Step 3: Write the per-volume report and the control**

Append to `cmd/undo/doctor.go`:

```go
// reportVolume prints what one target established. The two labels that are
// qualified here are qualified deliberately: reflink is a property of the
// filesystem that the shim does not yet use, and the budget is one global
// number, not a per-volume one. Dropping either qualifier would tell the
// reader something untrue.
func reportVolume(report func(checkState, string, string), name string, v volumeVerdict) {
	switch {
	case v.Lost:
		report(warn, "volume "+name, "nothing was saved for the canary here")
	case v.Fallback:
		report(warn, "volume "+name,
			"no directory you own on this filesystem, so backups go to the session store as "+
				"size-capped copies. Creating a directory of your own on this volume fixes it")
	default:
		free := "costing real bytes"
		if v.Method == "link" {
			free = "hardlinked, costing nothing"
		}
		report(pass, "volume "+name, fmt.Sprintf("store %s; deletions %s (%s)",
			v.StoreRoot, free, v.Method))
	}
	if v.ReflinkKnown {
		state := "no"
		if v.Reflink {
			state = "yes"
		}
		fmt.Printf("       reflink on this filesystem: %s (not yet used by the shim)\n", state)
	}
	fmt.Printf("       overwrite cap %s per file; store budget %s, global rather than per-volume\n",
		envOr("UNDO_MAX_BYTES", "256 MiB"), envOr("UNDO_MAX_STORE", "1 GiB"))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// controlPasses re-runs the round trip in the temporary directory, to separate
// "this volume cannot be protected" from "the shim is not working anywhere it
// was tried". It narrows a diagnosis; it does not prove causation -- both
// locations carry their own permissions, mount options and ignore rules.
func controlPasses(shim string) bool {
	dir, err := os.MkdirTemp("", "undo-doctor-control-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	v, err := checkVolume(shim, dir)
	return err == nil && v.Problem == ""
}
```

Delete `reportRoundTrip`, which `checkVolume` replaces.

- [ ] **Step 4: Fix the two pre-existing defects in this file**

In `reportStore`, replace the fixed-name probe:

```go
	probe := filepath.Join(root, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
```

with a unique one, so two concurrent doctors cannot remove or report on each
other's file:

```go
	probe, err := os.CreateTemp(root, ".doctor-probe-")
	if err != nil {
		report(failed, "store", "not writable: "+err.Error())
		return
	}
	probe.Close()
	os.Remove(probe.Name())
```

- [ ] **Step 5: Build, vet, and run the whole suite**

```bash
test/in-container.sh make test
```

Expected: `all cases passed`, with case 23 still green — it greps `[ok  ] capture` and `[ok  ] restore`, which Step 2 keeps.

- [ ] **Step 6: Commit**

```bash
git add cmd/undo/doctor.go cmd/undo/main.go
git commit -m "doctor: report per volume, and take a path to ask about"
```

---

### Task 5: End-to-end cases and documentation

**Files:**
- Modify: `test/e2e.sh` (above the banner on the last line)
- Modify: `test/multifs.sh`
- Modify: `README.md`

- [ ] **Step 1: Add the e2e cases**

Insert immediately **above** the `echo "all cases passed"` block at the end of `test/e2e.sh`, numbering from the current highest case:

```bash
echo "== case 53: doctor reports the volume it was asked about"
make_tree
out=$("$UNDO" doctor "$PLAY" 2>&1) || fail "doctor exited non-zero: $out"
grep -q "volume $PLAY" <<<"$out" || fail "case 53: doctor did not report the target volume"
grep -q "\[ok  \] capture" <<<"$out" || fail "case 53: capture label changed; case 23 depends on it"

echo "== case 54: a missing target fails that target and no more"
out=$("$UNDO" doctor "$WORK/definitely-not-here" 2>&1 || true)
grep -q "volume $WORK/definitely-not-here" <<<"$out" ||
    fail "case 54: a missing target was not reported"

echo "== case 55: a target whose name contains a space is not run through a shell"
spaced="$WORK/two words"
mkdir -p "$spaced"
out=$("$UNDO" doctor "$spaced" 2>&1) || fail "doctor exited non-zero: $out"
grep -q "\[ok  \] capture" <<<"$out" ||
    fail "case 55: a path with a space broke the round trip, so it is being interpolated"

echo "== case 56: doctor leaves nothing behind in the directory it probed"
before=$(ls -A "$spaced" | wc -l)
"$UNDO" doctor "$spaced" >/dev/null 2>&1 || true
[[ $(ls -A "$spaced" | wc -l) -eq $before ]] ||
    fail "case 56: doctor left files in the directory it probed"

echo "== case 57: an unwritable target is reported, and later targets still run"
ro=$WORK/readonly
mkdir -p "$ro"
chmod 500 "$ro"
out=$("$UNDO" doctor "$ro" "$PLAY" 2>&1 || true)
chmod 700 "$ro"
grep -q "volume $ro" <<<"$out" || fail "case 57: the unwritable target was not reported"
grep -q "volume $PLAY" <<<"$out" ||
    fail "case 57: a failing target stopped the targets after it being checked"

echo "== case 58: the answer follows the target, not TMPDIR"
# The bug this whole feature exists to fix: the round trip used to run in
# whatever TMPDIR named, which a batch system repoints per job.
tmpalt=$WORK/tmpalt
mkdir -p "$tmpalt"
out=$(TMPDIR=$tmpalt "$UNDO" doctor "$PLAY" 2>&1) || fail "doctor exited non-zero: $out"
grep -q "volume $PLAY" <<<"$out" || fail "case 58: doctor did not report the target"
[[ -z $(ls -A "$tmpalt") ]] ||
    fail "case 58: doctor wrote into TMPDIR instead of the target it was given"
```

Case 57 runs as root under `test/in-container.sh`, where mode 500 does not stop
root writing. Assert only that both targets appear in the output; do not assert
that the read-only one failed, or the case passes for the wrong reason as
non-root and fails as root.

- [ ] **Step 2: Run the suite**

```bash
test/in-container.sh make test
```

Expected: `all cases passed`, now through case 56.

- [ ] **Step 3: Add the two-filesystem case**

Do **not** add this inline to `test/multifs.sh`. That script is a harness: it
mounts two tmpfs, exports `FS_A` and `FS_B`, asserts the filesystem properties
the placement tests depend on, and then runs an assertions file passed as `$1`.
Its own cases deliberately use no binary, and it never runs `make`, so there is
no `bin/undo` to call from inside it.

Create `test/multifs-doctor.sh`:

```bash
#!/usr/bin/env bash
# Assertions for test/multifs.sh: doctor must answer per filesystem.
#
#   test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-doctor.sh'
#
# FS_A and FS_B are exported by the harness and are separate tmpfs mounts with
# distinct st_dev.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
UNDO=$ROOT/bin/undo
export UNDO_LIB=$ROOT/build/libundo.so
export UNDO_DATA_DIR=${UNDO_DATA_DIR:-/tmp/undo-multifs-store}
export UNDO_HOOK=multifs   # doctor treats a missing hook as a failure

fail() { echo "FAIL: $*" >&2; exit 1; }

storeroot() { # storeroot <doctor output>
    sed -n 's/.*store \([^;]*\);.*/\1/p' <<<"$1" | head -1
}

mkdir -p "$FS_A/work" "$FS_B/work"

echo "== multifs: doctor reports a different store root per filesystem"
a=$("$UNDO" doctor "$FS_A/work" 2>&1) || fail "doctor failed on the first filesystem: $a"
b=$("$UNDO" doctor "$FS_B/work" 2>&1) || fail "doctor failed on the second: $b"
roota=$(storeroot "$a")
rootb=$(storeroot "$b")
[[ -n $roota ]] || fail "no store root reported for the first filesystem: $a"
[[ -n $rootb ]] || fail "no store root reported for the second: $b"
[[ $roota != "$rootb" ]] ||
    fail "both filesystems reported the same store root ($roota); placement is not per-filesystem"

echo "== multifs: both store roots lie on the filesystem they describe"
[[ $roota == "$FS_A"* ]] || fail "first store root $roota is not under $FS_A"
[[ $rootb == "$FS_B"* ]] || fail "second store root $rootb is not under $FS_B"

echo
echo "multifs doctor assertions ok"
```

Make it executable: `chmod +x test/multifs-doctor.sh`.

- [ ] **Step 4: Run the two-filesystem harness**

```bash
test/in-container.sh --privileged bash -c 'make && test/multifs.sh test/multifs-doctor.sh'
```

Expected: the harness's own cases pass, then `multifs doctor assertions ok`.

- [ ] **Step 5: Document it**

In `README.md`, change the usage block's doctor line to:

```
undo doctor [path...]  check the install, and whether files are protected here
```

and add, under the existing doctor paragraph in "Check it works":

```markdown
`doctor` answers for the directory you run it in, because the answer differs
per filesystem: a deletion is a free hardlink only when undo can put its store
on the same filesystem as the file. Name other directories to ask about them
too:

```console
$ undo doctor ~ /scratch
```

A volume reported as using the session store has no directory you own on it,
so backups there are size-capped copies rather than free hardlinks.
```

- [ ] **Step 6: Scan and commit**

```bash
tools/check-no-site-data.sh test/e2e.sh test/multifs-doctor.sh README.md
git add test/e2e.sh test/multifs-doctor.sh README.md
git commit -m "test: doctor answers per volume, and says so in the README"
```

---

## Notes for the implementer

- **Two cases the spec lists are deliberately not in this plan**, rather than
  half-done:
  - *The group-owned-parent fallback.* It needs a directory owned by a
    different uid, which the e2e suite cannot arrange portably — it runs as
    root in a container, where ownership tests do not discriminate. It belongs
    in the `--privileged` multifs harness, which can create a second uid, and
    it is worth a plan of its own. It is the regression test for the central
    correction in the design, so it should not be left indefinitely.
  - *The control path* (shim present but capture failing). Making capture fail
    while leaving the shim loadable needs a deliberately broken store, which is
    a fixture this suite does not have yet.
- **The output format is load-bearing for tests.** `test/multifs-doctor.sh`
  parses `store <path>;` out of the volume line. If you change that phrasing,
  change the `storeroot` helper with it.
- **If `make test` goes red, you broke it.** The baseline is green on aarch64
  at the commit this plan was written against.
