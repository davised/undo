// Package journal parses the shim's journal files.
//
// Each line is: op<TAB>field<TAB>field... with %, control bytes and DEL
// percent-encoded by the shim.
package journal

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Op types written by the shim.
const (
	OpCreate   = "create"   // path            -> remove it
	OpUnlink   = "unlink"   // path, backup    -> move backup back
	OpRmlink   = "rmlink"   // path, target    -> recreate symlink
	OpMod      = "mod"      // path, backup    -> move backup back
	OpRename   = "rename"   // old, new, backup|- -> rename back, restore target
	OpExchange = "exchange" // a, b            -> swap back
	OpMkdir    = "mkdir"    // path            -> rmdir
	OpRmdir    = "rmdir"    // path, mode      -> mkdir
	OpChmod    = "chmod"    // path, old, new  -> chmod back
	OpLost     = "lost"     // path, why       -> warn only
)

type Entry struct {
	Op     string
	Fields []string
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

// Read parses a journal file. Unknown ops are kept as-is so newer shims
// degrade gracefully.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 2 {
			continue
		}
		e := Entry{Op: decode(parts[0])}
		for _, p := range parts[1:] {
			e.Fields = append(e.Fields, decode(p))
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
	default:
		return e.Op + " " + strings.Join(e.Fields, " ")
	}
}
