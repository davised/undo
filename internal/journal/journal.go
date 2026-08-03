// Package journal parses the shim's journal files.
//
// Each line is: op<TAB>field<TAB>field... with %, control bytes and DEL
// percent-encoded by the shim.
package journal

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Op types written by the shim.
const (
	OpCreate    = "create"   // path            -> remove it
	OpUnlink    = "unlink"   // path, backup    -> move backup back
	OpRmlink    = "rmlink"   // path, target    -> recreate symlink
	OpMod       = "mod"      // path, backup    -> move backup back
	OpRename    = "rename"   // old, new, backup|- -> rename back, restore target
	OpExchange  = "exchange" // a, b            -> swap back
	OpMkdir     = "mkdir"    // path            -> rmdir
	OpRmdir     = "rmdir"    // path, mode      -> mkdir
	OpChmod     = "chmod"    // path, old, new  -> chmod back
	OpLost      = "lost"     // path, why       -> warn only
	OpStoreMove = "storemv"  // old-prefix, new-prefix|- -> backups moved
)

type Entry struct {
	Op      string
	Fields  []string
	Corrupt bool // failed its integrity check; keeps its slot, never acted on
}

// backupField returns the index of e's backup path field, or -1 if the op
// carries no backup.
func (e Entry) backupField() int {
	switch e.Op {
	case OpUnlink, OpMod:
		return 1
	case OpRename:
		return 2
	}
	return -1
}

// Backup returns the backup path this entry names, or "" when it names none
// and "-" when the backup was discarded.
//
// Every consumer that needs a backup path goes through this: which field holds
// it differs per op, and duplicating that knowledge in the session and restore
// packages is how the three drift apart when a new op is added.
func (e Entry) Backup() string {
	i := e.backupField()
	if i < 0 || i >= len(e.Fields) {
		return ""
	}
	return e.Fields[i]
}

// Method reports how the shim saved this entry's backup: "link" for a
// hardlink, which allocates nothing, "copy" for a full byte copy, or "none"
// when no backup was taken.
//
// A record with no method field predates the field and is read as "copy" --
// the pessimistic answer, and exactly the accounting those journals were
// written under.
func (e Entry) Method() string {
	i := e.backupField()
	if i < 0 {
		return "none"
	}
	if i+1 < len(e.Fields) {
		if m := e.Fields[i+1]; m != "" {
			return m
		}
	}
	if i < len(e.Fields) && (e.Fields[i] == "" || e.Fields[i] == "-") {
		return "none"
	}
	return "copy"
}

// hasPrefixPath reports whether path is pfx or lies beneath it, comparing on
// '/' boundaries so /v/p does not match /v/p2.
func hasPrefixPath(path, pfx string) bool {
	if path == pfx {
		return true
	}
	return len(path) > len(pfx) && strings.HasPrefix(path, pfx) &&
		path[len(pfx)] == '/'
}

// ResolveStoreMoves rewrites backup paths through the storemv records the shim
// appends when it has to move a store out of a directory being removed.
//
// A storemv only affects backups recorded before it: anything saved afterwards
// already went to the new location. A destination of "-" means the store could
// not be moved and was discarded, so those backups become "-" and restore
// reports them as gone rather than chasing a path that no longer exists.
//
// The returned slice has the same length and order as the input. Journal
// indices are load-bearing -- restore.slot() and --only are keyed by position
// -- so storemv records stay in place rather than being filtered out.
func ResolveStoreMoves(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	for i, e := range out {
		if e.Op != OpStoreMove || len(e.Fields) < 2 {
			continue
		}
		from, to := e.Fields[0], e.Fields[1]
		if from == "" {
			continue
		}
		for j := 0; j < i; j++ {
			b := out[j].backupField()
			if b < 0 || b >= len(out[j].Fields) {
				continue
			}
			p := out[j].Fields[b]
			if p == "" || p == "-" || !hasPrefixPath(p, from) {
				continue
			}
			fields := make([]string, len(out[j].Fields))
			copy(fields, out[j].Fields)
			if to == "-" {
				fields[b] = "-"
			} else {
				fields[b] = to + p[len(from):]
			}
			out[j].Fields = fields
		}
	}
	return out
}

func decode(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v byte
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02X", &v); err == nil {
				b.WriteByte(v)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// integrityLen is the width of the trailing integrity field: '~' and sixteen
// hex digits.
const integrityLen = 17

// versionFile sits beside a journal and declares that every record in it
// carries an integrity field.
//
// Per journal rather than per record, and that is the whole point. Inferring
// it -- "no trailing marker means this record predates the field" -- accepts a
// record that a short write truncated before its field, which is precisely the
// corruption the field exists to catch. A truncated record cannot erase this
// file: it is written once at session setup, not per record.
const versionFile = "journalv"

// fnv1a is the 64-bit FNV-1a the shim writes over each encoded record.
//
// Non-cryptographic on purpose: this detects a truncated record and the
// concatenation of two records, not tampering. An attacker who can write the
// journal can write anything in it.
//
// The offset basis is 14695981039346656037, the real FNV-1a 64 constant. Do
// not copy it from the shim's path_hash, which uses 1469598103934665603 -- one
// digit short, harmless for a hash table's slot index and wrong here, and a
// mismatch means every record the shim writes is rejected as corrupt.
func fnv1a(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// validIntegrity reports whether field is the integrity field for body.
func validIntegrity(field, body string) bool {
	if len(field) != integrityLen || field[0] != '~' {
		return false
	}
	want, err := strconv.ParseUint(field[1:], 16, 64)
	if err != nil {
		return false
	}
	return want == fnv1a(body)
}

// parseLine turns one journal line into an entry.
//
// The bool is false only for a line that is not a record at all, and what
// qualifies differs by journal:
//
// In a versioned journal that means an empty line and nothing else. Every
// other non-empty line was written by a shim that promised an integrity field,
// so a line too short even to hold a tab is a record a short write cut off
// mid-first-field -- damaged, not absent. Dropping it would shift every index
// after it, and slot() is keyed by position, so later valid entries would
// resolve to the wrong parked files.
//
// A legacy journal keeps the historical skip of any line with fewer than two
// fields. There the shim promised nothing, and changing the rule would
// renumber journals already on disk.
func parseLine(line string, verify bool) (Entry, bool) {
	parts := strings.Split(line, "\t")
	if verify {
		if line == "" {
			return Entry{}, false
		}
		// Op is kept so the entry describes itself in a listing; the fields
		// are deliberately not, because they are the untrustworthy part and
		// nothing should be tempted to read them.
		corrupt := Entry{Op: decode(parts[0]), Corrupt: true}
		if len(parts) < 2 {
			return corrupt, true
		}
		last := parts[len(parts)-1]
		// everything before the final tab, which is what the shim hashed
		body := line[:len(line)-len(last)-1]
		if !validIntegrity(last, body) {
			return corrupt, true
		}
		parts = parts[:len(parts)-1]
	} else if len(parts) < 2 {
		return Entry{}, false
	}
	e := Entry{Op: decode(parts[0])}
	for _, p := range parts[1:] {
		e.Fields = append(e.Fields, decode(p))
	}
	return e, true
}

// Read parses a journal file. Unknown ops are kept as-is so newer shims
// degrade gracefully.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Whether records must validate is declared by the session, not guessed
	// per record. See versionFile.
	_, verr := os.Stat(filepath.Join(filepath.Dir(path), versionFile))
	verify := verr == nil

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		e, ok := parseLine(sc.Text(), verify)
		if !ok {
			continue
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// Describe returns a one-line human description of an entry.
func (e Entry) Describe() string {
	f := func(i int) string {
		if i < len(e.Fields) {
			return e.Fields[i]
		}
		return "?"
	}
	if e.Corrupt {
		return "CORRUPT   " + e.Op + " record (integrity check failed)"
	}
	switch e.Op {
	case OpCreate:
		return "created   " + f(0)
	case OpUnlink:
		return "deleted   " + f(0)
	case OpRmlink:
		return "deleted   " + f(0) + " (symlink -> " + f(1) + ")"
	case OpMod:
		return "modified  " + f(0)
	case OpRename:
		return "moved     " + f(0) + " -> " + f(1)
	case OpExchange:
		return "swapped   " + f(0) + " <-> " + f(1)
	case OpMkdir:
		return "created   " + f(0) + "/"
	case OpRmdir:
		return "deleted   " + f(0) + "/"
	case OpChmod:
		return "mode      " + f(0) + " (" + f(1) + " -> " + f(2) + ")"
	case OpLost:
		return "changed   " + f(0) + " (no backup saved)"
	case OpStoreMove:
		return "store     " + f(0) + " -> " + f(1)
	default:
		return e.Op + " " + strings.Join(e.Fields, " ")
	}
}
