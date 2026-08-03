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
