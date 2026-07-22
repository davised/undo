package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/edaywalid/undo/internal/journal"
	"github.com/edaywalid/undo/internal/session"
)

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// contentDiff shells out to diff -u between the pre-command backup and
// the current file.
func contentDiff(before, after, label string) {
	diffBin, err := exec.LookPath("diff")
	if err != nil {
		fmt.Println("    (diff not available)")
		return
	}
	cmd := exec.Command(diffBin, "-u",
		"--label", "before/"+label, "--label", "after/"+label, before, after)
	out, err := cmd.Output()
	switch {
	case err == nil:
		fmt.Println("    (no content change)")
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1:
		fmt.Print(string(out))
	default:
		fmt.Println("    (could not diff:", err, ")")
	}
}

func cmdDiff(args []string) {
	var s *session.Session
	var err error
	if len(args) > 0 {
		s, err = session.Get(args[0])
	} else {
		s, err = session.Latest()
		if err != nil {
			// fall back to the newest session even if undone
			if all, lerr := session.List(); lerr == nil {
				for _, c := range all {
					if len(c.Entries) > 0 {
						s, err = c, nil
						break
					}
				}
			}
		}
	}
	if err != nil || s == nil {
		fatal(fmt.Errorf("no such session"))
	}

	fmt.Printf("session %s (%s)\n$ %s\n\n", shortID(s.ID), when(s.ID), s.Cmd)
	if s.Undone {
		fmt.Println("session is currently undone; describing the original changes")
		fmt.Println()
	}

	for _, e := range s.Entries {
		f := func(i int) string {
			if i < len(e.Fields) {
				return e.Fields[i]
			}
			return ""
		}
		fmt.Println(e.Describe())
		if s.Undone {
			continue // backups have been swapped around, sides would lie
		}
		switch e.Op {
		case journal.OpMod:
			contentDiff(f(1), f(0), f(0))
		case journal.OpRename:
			if f(2) != "-" && f(2) != "" {
				fmt.Println("  overwrote the previous content of", f(1), ":")
				contentDiff(f(2), f(1), f(1))
			}
		case journal.OpUnlink:
			if st, err := os.Stat(f(1)); err == nil {
				fmt.Printf("    (%s saved in backup)\n", humanSize(st.Size()))
			}
		}
	}
}
