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
  undo -i                 pick a session and entries interactively
  undo apply <id> [flags] revert a specific session
  undo redo [id] [flags]  re-apply an undone session
  undo diff [id]          show what a session changed, with content diffs
  undo run [--] <cmd...>  run one command with the shim armed (no hook needed)
  undo list               list recent sessions
  undo show [id]          show what a session changed

flags:
  -n, --dry-run   show what would happen without doing it
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
	return time.Unix(n, 0).Format("Jan 2 15:04:05")
}

func shortID(id string) string {
	if len(id) > 14 {
		return id[:14]
	}
	return id
}

func cmdList() {
	sessions, err := session.List()
	if err != nil {
		fatal(err)
	}
	shown := 0
	for _, s := range sessions {
		if len(s.Entries) == 0 {
			continue
		}
		shown++
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
	if shown == 0 {
		fmt.Println("no sessions recorded (is the shell hook sourced?)")
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
	for i, e := range s.Entries {
		fmt.Printf("  %2d  %s\n", i+1, e.Describe())
	}
	if s.Undone {
		fmt.Println("\ncurrently undone (undo redo to re-apply)")
	}
}

// one shared reader so consecutive prompts don't swallow each other's
// buffered input
var stdin = bufio.NewReader(os.Stdin)

func readLine(prompt string) string {
	fmt.Print(prompt)
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func confirm(prompt string) bool {
	a := strings.ToLower(readLine(prompt))
	return a == "y" || a == "yes"
}

func previewSession(s *session.Session, full bool) {
	fmt.Printf("$ %s  (%s, %d changes)\n", s.Cmd, when(s.ID), len(s.Entries))
	show := s.Entries
	if len(show) > 10 && !full {
		show = show[:10]
	}
	for _, e := range show {
		fmt.Println("  " + e.Describe())
	}
	if n := len(s.Entries) - len(show); n > 0 {
		fmt.Printf("  ... and %d more (undo show %s)\n", n, shortID(s.ID))
	}
}

func report(res *restore.Result, opts restore.Options, dir restore.Direction) {
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
		verb := "restored"
		if dir == restore.Redo {
			verb = "re-applied"
		}
		fmt.Printf("%s %d change(s)\n", verb, res.Done)
	}
}

func cmdApply(s *session.Session, dir restore.Direction, opts restore.Options, yes bool) {
	if len(s.Entries) == 0 {
		fatal(fmt.Errorf("session recorded no changes"))
	}
	if dir == restore.Undo && s.Undone {
		fatal(fmt.Errorf("session was already undone (undo redo %s to re-apply)", shortID(s.ID)))
	}
	if dir == restore.Redo && !s.Undone {
		fatal(fmt.Errorf("session is not undone, nothing to redo"))
	}

	previewSession(s, opts.DryRun)

	if !opts.DryRun && !yes {
		prompt := "\nrevert this? [y/N] "
		if dir == restore.Redo {
			prompt = "\nre-apply this? [y/N] "
		}
		if !confirm(prompt) {
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

func main() {
	// `undo run` takes the rest of argv verbatim, before any flag parsing
	if len(os.Args) > 1 && os.Args[1] == "run" {
		cmdRun(os.Args[2:])
		return
	}

	var opts restore.Options
	var yes, interactive bool
	var args []string

	for _, a := range os.Args[1:] {
		switch a {
		case "-n", "--dry-run":
			opts.DryRun = true
		case "-y", "--yes":
			yes = true
		case "--force":
			opts.Force = true
		case "-i", "--interactive":
			interactive = true
		case "-h", "--help", "help":
			fmt.Print(usage)
			return
		default:
			args = append(args, a)
		}
	}

	if interactive {
		cmdInteractive(opts, yes)
		return
	}

	if len(args) == 0 {
		s, err := session.Latest()
		if err != nil {
			fatal(fmt.Errorf("nothing to undo"))
		}
		cmdApply(s, restore.Undo, opts, yes)
		return
	}

	switch args[0] {
	case "list", "ls":
		cmdList()
	case "show":
		cmdShow(args[1:])
	case "diff":
		cmdDiff(args[1:])
	case "apply":
		if len(args) < 2 {
			fatal(fmt.Errorf("apply needs a session id"))
		}
		s, err := session.Get(args[1])
		if err != nil {
			fatal(fmt.Errorf("no such session %q", args[1]))
		}
		cmdApply(s, restore.Undo, opts, yes)
	case "redo":
		var s *session.Session
		var err error
		if len(args) > 1 {
			s, err = session.Get(args[1])
		} else {
			s, err = session.LatestUndone()
		}
		if err != nil {
			fatal(fmt.Errorf("nothing to redo"))
		}
		cmdApply(s, restore.Redo, opts, yes)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}
