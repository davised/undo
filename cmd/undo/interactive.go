package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/edaywalid/undo/internal/restore"
	"github.com/edaywalid/undo/internal/session"
)

// parseSelection turns "1,3-5" into a set of journal indices (0-based).
func parseSelection(input string, max int) (map[int]bool, error) {
	sel := make(map[int]bool)
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if i := strings.IndexByte(part, '-'); i > 0 {
			lo, hi = part[:i], part[i+1:]
		}
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad selection %q", part)
		}
		b, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return nil, fmt.Errorf("bad selection %q", part)
		}
		for n := a; n <= b; n++ {
			if n < 1 || n > max {
				return nil, fmt.Errorf("%d is out of range 1-%d", n, max)
			}
			sel[n-1] = true
		}
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("empty selection")
	}
	return sel, nil
}

func cmdInteractive(opts restore.Options, yes bool) {
	all, err := session.List()
	if err != nil {
		fatal(err)
	}
	var sessions []*session.Session
	for _, s := range all {
		if len(s.Entries) > 0 {
			sessions = append(sessions, s)
		}
	}
	if len(sessions) == 0 {
		fatal(fmt.Errorf("nothing to undo"))
	}
	if len(sessions) > 15 {
		sessions = sessions[:15]
	}

	for i, s := range sessions {
		mark := " "
		if s.Undone {
			mark = "u"
		}
		cmd := s.Cmd
		if len(cmd) > 55 {
			cmd = cmd[:52] + "..."
		}
		fmt.Printf("%2d %s %s  %3d changes  %s\n", i+1, mark, when(s.ID), len(s.Entries), cmd)
	}

	choice := readLine("\nsession [1]: ")
	idx := 1
	if choice != "" {
		if idx, err = strconv.Atoi(choice); err != nil || idx < 1 || idx > len(sessions) {
			fatal(fmt.Errorf("invalid session number"))
		}
	}
	s := sessions[idx-1]

	dir := restore.Undo
	verb := "revert"
	if s.Undone {
		dir = restore.Redo
		verb = "re-apply"
		fmt.Println("\nthis session is undone; selecting it will re-apply the changes")
	}

	fmt.Printf("\n$ %s\n", s.Cmd)
	for i, e := range s.Entries {
		fmt.Printf("  %2d  %s\n", i+1, e.Describe())
	}

	sel := readLine(fmt.Sprintf("\nentries to %s (e.g. 1,3-5) [all]: ", verb))
	if sel != "" && !strings.EqualFold(sel, "all") {
		only, err := parseSelection(sel, len(s.Entries))
		if err != nil {
			fatal(err)
		}
		opts.Only = only
		fmt.Printf("cherry-picking %d of %d entries (session state will not change)\n",
			len(only), len(s.Entries))
	}

	if !opts.DryRun && !yes {
		if !confirm(fmt.Sprintf("%s? [y/N] ", verb)) {
			fmt.Println("aborted")
			return
		}
	}

	res, err := restore.Run(s, dir, opts)
	if err != nil {
		fatal(err)
	}
	report(res, opts, dir)
}
