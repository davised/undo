package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edaywalid/undo/internal/journal"
	"github.com/edaywalid/undo/internal/restore"
	"github.com/edaywalid/undo/internal/session"
)

// volumeVerdict is what one target directory's round trip established.
//
// Problem is empty when the record was classified. When set, it is the reason
// the volume could not be judged, and the caller reports FAIL rather than
// guessing at a healthy answer.
type volumeVerdict struct {
	StoreRoot    string // "" when the backup did not land in a filesystem-local store
	Fallback     bool   // the backup went to the session store instead
	Method       string // link, copy, none, or an unrecognised token verbatim
	Lost         bool   // nothing was saved for the canary
	Problem      string
	Reflink      bool // the target's filesystem can clone extents
	ReflinkKnown bool // the probe produced an answer at all
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
		// A corrupt record cannot match: journal.Read keeps its Op and drops
		// its fields on purpose, so there is no victim path left to compare.
		// Without this, a journal whose canary record failed its integrity
		// check reads as "the shim recorded nothing" -- blaming the shim for
		// what is a journal problem.
		for _, e := range entries {
			if e.Corrupt {
				return volumeVerdict{Problem: "a journal record failed its integrity check"}
			}
		}
		return volumeVerdict{Problem: "the shim recorded no deletion of the canary"}
	case len(found) > 1:
		return volumeVerdict{Problem: fmt.Sprintf(
			"%d records for one deletion; the journal disagrees with itself", len(found))}
	}
	e := found[0]
	// Defence for a future reader that preserves fields: a corrupt record that
	// matched would have untrustworthy fields, so it must not be trusted. Not
	// reachable today -- journal.Read drops a corrupt record's fields, so it
	// never matches -- but keep guarding the matched path regardless.
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
	defer func() {
		// Remove finds distributed backups through the session's journal
		// entries, and session.Create returns a session with none. Reload it,
		// or a store this round trip placed on the target filesystem outlives
		// the diagnostic: every path that skips the restore -- a corrupt
		// journal, duplicate records, an unexpected backup location -- would
		// otherwise orphan <root>/.undo/<id>/ in the user's own tree.
		//
		// Safe after a successful restore too: the backup has already moved
		// back by then, so there is nothing left to unlink and this is a
		// no-op. Nothing outside our own store is reachable either way --
		// removeDistributedBackups checks both the path's shape against this
		// session's id and what is actually on disk before unlinking.
		if fresh, err := session.Get(sess.ID); err == nil {
			fresh.Remove()
			return
		}
		sess.Remove()
	}()

	// The victim is passed as an argument, never concatenated into the script:
	// once a user-supplied path reaches here, a space or a metacharacter would
	// otherwise change what runs.
	cmd := exec.Command("/bin/sh", "-c", `rm -- "$1"`, "sh", victim)
	cmd.Env = armedEnv(os.Environ(), shim, sess.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		// A Problem, not an error: the shim was reached, so the control in
		// doctor can still say whether this volume is the difference. An
		// error here means the target was never usable at all, and the
		// control has nothing to compare against.
		return volumeVerdict{Problem: fmt.Sprintf(
			"the canary could not be deleted: %v %s", err, strings.TrimSpace(string(out)))}, nil
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
