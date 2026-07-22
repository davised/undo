// undo reverts the filesystem changes made by a previous shell command.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edaywalid/undo/internal/restore"
	"github.com/edaywalid/undo/internal/session"
)

const usage = `undo - revert what the last command did to the filesystem

usage:
  undo [flags]            revert the most recent command that changed files
  undo apply <id> [flags] revert a specific session
  undo list               list recent sessions
  undo show [id]          show what a session changed

flags:
  -n, --dry-run   show what would be restored without doing it
  -y, --yes       skip the confirmation prompt
      --force     overwrite files that changed after the session
`

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "undo:", err)
	os.Exit(1)
}

func when(id string) string {
	sec := id
	if len(sec) > 10 {
		sec = sec[:10]
	}
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return "?"
	}
	return time.Unix(n, 0).Format("15:04:05")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func cmdList() {
	sessions, err := session.List()
	if err != nil {
		fatal(err)
	}
	if len(sessions) == 0 {
		fmt.Println("no sessions recorded (is the zsh hook sourced?)")
		return
	}
	for _, s := range sessions {
		mark := " "
		if s.Undone {
			mark = "u"
		}
		cmd := s.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		fmt.Printf("%s %s  %s  %3d changes  %s\n",
			mark, shortID(s.ID), when(s.ID), len(s.Entries), cmd)
	}
}

func cmdShow(args []string) {
	var s *session.Session
	var err error
	if len(args) > 0 {
		s, err = session.Get(args[0])
	} else {
		s, err = session.Latest()
	}
	if err != nil {
		fatal(fmt.Errorf("no such session"))
	}
	fmt.Printf("session %s (%s)\n$ %s\n\n", shortID(s.ID), when(s.ID), s.Cmd)
	for _, e := range s.Entries {
		fmt.Println("  " + e.Describe())
	}
	if s.Undone {
		fmt.Println("\nalready undone")
	}
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(sc.Text()))
	return a == "y" || a == "yes"
}

func cmdApply(s *session.Session, opts restore.Options, yes bool) {
	if len(s.Entries) == 0 {
		fatal(fmt.Errorf("session recorded no changes"))
	}
	if s.Undone {
		fatal(fmt.Errorf("session was already undone"))
	}

	fmt.Printf("$ %s  (%s, %d changes)\n", s.Cmd, when(s.ID), len(s.Entries))
	show := s.Entries
	if len(show) > 10 && !opts.DryRun {
		show = show[:10]
	}
	for _, e := range show {
		fmt.Println("  " + e.Describe())
	}
	if n := len(s.Entries) - len(show); n > 0 {
		fmt.Printf("  ... and %d more (undo show %s)\n", n, shortID(s.ID))
	}

	if !opts.DryRun && !yes {
		if !confirm("\nrevert this? [y/N] ") {
			fmt.Println("aborted")
			return
		}
	}

	res, err := restore.Run(s, opts)
	if err != nil {
		fatal(err)
	}
	if opts.DryRun {
		fmt.Println()
		for _, a := range res.Actions {
			fmt.Println(a)
		}
	}
	for _, w := range res.Skipped {
		fmt.Fprintln(os.Stderr, "skipped:", w)
	}
	if !opts.DryRun {
		fmt.Printf("restored %d change(s)\n", res.Done)
	}
}

func main() {
	var opts restore.Options
	var yes bool
	var args []string

	for _, a := range os.Args[1:] {
		switch a {
		case "-n", "--dry-run":
			opts.DryRun = true
		case "-y", "--yes":
			yes = true
		case "--force":
			opts.Force = true
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		default:
			args = append(args, a)
		}
	}

	if len(args) == 0 {
		s, err := session.Latest()
		if err != nil {
			fatal(fmt.Errorf("nothing to undo"))
		}
		cmdApply(s, opts, yes)
		return
	}

	switch args[0] {
	case "list", "ls":
		cmdList()
	case "show":
		cmdShow(args[1:])
	case "apply":
		if len(args) < 2 {
			fatal(fmt.Errorf("apply needs a session id"))
		}
		s, err := session.Get(args[1])
		if err != nil {
			fatal(fmt.Errorf("no such session %q", args[1]))
		}
		cmdApply(s, opts, yes)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}
