package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/edaywalid/undo/internal/session"
)

type checkState int

const (
	pass checkState = iota
	warn
	failed
)

func (s checkState) mark() string {
	switch s {
	case pass:
		return "ok  "
	case warn:
		return "warn"
	default:
		return "FAIL"
	}
}

// cmdDoctor runs environment checks and a live capture/restore round trip,
// so "nothing happened" turns into a concrete diagnosis.
func cmdDoctor(targets []string) {
	var worst checkState
	report := func(state checkState, name, detail string) {
		if state > worst {
			worst = state
		}
		line := fmt.Sprintf("[%s] %s", state.mark(), name)
		if detail != "" {
			line += ": " + detail
		}
		fmt.Println(line)
	}

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

	fmt.Println("undo doctor")
	fmt.Println()

	// 1. shim located
	shim := findShim()
	if shim == "" {
		report(failed, "shim", "libundo.so not found; set UNDO_LIB or reinstall")
	} else {
		report(pass, "shim", shim)
	}

	// 2. libc flavor
	if musl, note := detectLibc(); musl {
		report(warn, "libc", note)
	} else {
		report(pass, "libc", note)
	}

	// 3. store present, private, writable
	root := session.Root()
	reportStore(report, root)

	// 4. ignore configuration
	reportIgnore(report)

	// 5. hooks installed
	reportHooks(report, shim)

	// 6 and 7. live capture + restore round trip
	if shim != "" {
		controlRun, controlOK := false, false
		for _, t := range targets {
			v, err := checkVolume(shim, t)
			if err != nil {
				// never reached the shim, so the control would say nothing
				report(failed, "volume "+safeLabel(t), safeLabel(err.Error()))
				continue
			}
			// The literal labels below are asserted by test/e2e.sh case 23.
			if v.Problem != "" {
				if !controlRun {
					controlRun, controlOK = true, controlPasses(shim)
				}
				if controlOK {
					report(failed, "capture "+safeLabel(t), safeLabel(v.Problem)+
						"; the same check passes in the temporary directory, so this volume is the difference")
				} else {
					report(failed, "capture "+safeLabel(t), safeLabel(v.Problem)+
						"; it also fails in the temporary directory, so this volume is not implicated")
				}
				continue
			}
			report(pass, "capture", "1 change recorded")
			if v.Lost {
				// checkVolume skips the restore when nothing was saved, so there is no
				// result to report. Saying so beats omitting the line: an absence is
				// not something a reader should have to notice.
				report(warn, "restore", "not attempted: there was no backup to restore from")
			} else {
				report(pass, "restore", "canary recovered intact")
			}
			reportVolume(report, t, v)
		}
	}

	fmt.Println()
	switch worst {
	case pass:
		// separate lines: chained with && the shell makes it one command,
		// and undo will not revert a command it is running inside
		fmt.Println("all good. try it, one command per line:")
		fmt.Println("  touch x")
		fmt.Println("  rm x")
		fmt.Println("  undo")
	case warn:
		fmt.Println("usable, with warnings above.")
	default:
		fmt.Println("not working yet. fix the FAIL lines above.")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("arming:")
	arm := os.Getenv("UNDO_ARM")
	switch {
	case arm == "":
		fmt.Println("  UNDO_ARM       not set (env arming off; hooks may still be active)")
	case !strings.Contains(arm, ":"):
		fmt.Println("  UNDO_ARM       set, but with no identity (UNDO_ARM=1)")
		fmt.Println("                 the armer-exclusion and detach tests are DISABLED")
	default:
		fmt.Println("  UNDO_ARM      ", arm)
	}
	if p := os.Getenv("LD_PRELOAD"); strings.Contains(p, "libundo.so") {
		fmt.Println("  LD_PRELOAD     shim loaded")
	} else {
		fmt.Println("  LD_PRELOAD     shim NOT loaded; nothing in this process is captured")
	}
	if sid := os.Getenv("UNDO_SID"); sid != "" {
		fmt.Println("  UNDO_SID      ", sid, "(detach test active)")
	} else {
		fmt.Println("  UNDO_SID       not set; an inherited UNDO_SESSION is trusted unconditionally")
	}
	if pgid := selfStatField(5); pgid != "" && strings.HasPrefix(arm, pgid+":") {
		fmt.Println("  process group  same as the armer's: capture is DISABLED here.")
		fmt.Println("                 expected for the agent process itself; if every")
		fmt.Println("                 command reports this, nothing creates process groups")
		fmt.Println("                 (a container with no job control) and nothing is captured.")
	}
	ign := os.Getenv("UNDO_IGNORE")
	if ign == "" {
		ign = "(none beyond the built-in defaults)"
	}
	fmt.Println("  UNDO_IGNORE   ", ign)
}

func detectLibc() (musl bool, note string) {
	// musl ships an ld-musl loader; glibc ships ld-linux
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	if len(matches) == 0 {
		matches, _ = filepath.Glob("/usr/lib/ld-musl-*")
	}
	if len(matches) > 0 {
		return true, "musl detected; prebuilt shim targets glibc, build from source"
	}
	return false, "glibc"
}

func reportStore(report func(checkState, string, string), root string) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		report(failed, "store", err.Error())
		return
	}
	// writability probe
	probe, err := os.CreateTemp(root, ".doctor-probe-")
	if err != nil {
		report(failed, "store", "not writable: "+err.Error())
		return
	}
	probe.Close()
	os.Remove(probe.Name())

	// Only the sessions dir holds backups, so it is the one that must be
	// private. Its parent also holds the hook scripts and is world-readable
	// by design in both package and installer layouts.
	if fi, err := os.Stat(root); err == nil && fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			report(warn, "store", fmt.Sprintf("%s is mode %o, want 700", root, fi.Mode().Perm()))
			return
		}
	}
	report(pass, "store", root)
}

func reportIgnore(report func(checkState, string, string)) {
	defaults := "node_modules, .cache, __pycache__, .git (built in)"
	if extra := os.Getenv("UNDO_IGNORE"); extra != "" {
		n := len(strings.Split(extra, ":"))
		report(pass, "ignore", fmt.Sprintf("%d extra pattern(s) from config; %s", n, defaults))
	} else {
		report(pass, "ignore", defaults)
	}
}

// hookDir finds the installed hook scripts, trying the layout beside the
// shim first and the source tree second.
func hookDir(shim string) string {
	if shim == "" {
		return ""
	}
	candidates := []string{
		// .../lib/undo/libundo.so -> .../share/undo/
		filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(shim))), "share", "undo"),
		// dev tree: .../build/libundo.so -> .../shell/
		filepath.Join(filepath.Dir(filepath.Dir(shim)), "shell"),
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "undo.zsh")); err == nil {
			return dir
		}
	}
	return ""
}

func reportHooks(report func(checkState, string, string), shim string) {
	// What matters is whether the hook is loaded in this shell, not where
	// the files sit. The hook exports UNDO_HOOK when it loads; without it
	// nothing is ever recorded and every undo says "nothing to undo".
	if shell := os.Getenv("UNDO_HOOK"); shell != "" {
		report(pass, "hook", "active in this shell ("+shell+")")
		return
	}
	dir := hookDir(shim)
	if dir == "" {
		report(failed, "hook",
			"NOT active, and the hook scripts were not found. Reinstall undo.")
		return
	}
	report(failed, "hook", fmt.Sprintf(
		"NOT active in this shell, so nothing is being recorded.\n"+
			"         add one line to your shell rc, then open a new terminal:\n"+
			"           zsh:  echo 'source %s/undo.zsh'  >> ~/.zshrc\n"+
			"           bash: echo 'source %s/undo.bash' >> ~/.bashrc\n"+
			"           fish: echo 'source %s/undo.fish' >> ~/.config/fish/config.fish",
		dir, dir, dir))
}

// safeLabel makes a path safe to print in a report line. Filenames may contain
// newlines and control bytes, and doctor's output is read by people and grepped
// by the e2e suite: an embedded newline lets a directory name forge a line that
// looks like a passing check. Ordinary paths pass through untouched, so the
// output does not suddenly grow quotes around every path.
func safeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// reportVolume prints what one target established. The two labels that are
// qualified here are qualified deliberately: reflink is a property of the
// filesystem that the shim does not yet use, and the budget is one global
// number, not a per-volume one. Dropping either qualifier would tell the
// reader something untrue.
func reportVolume(report func(checkState, string, string), name string, v volumeVerdict) {
	switch {
	case v.Lost:
		report(warn, "volume "+safeLabel(name), "nothing was saved for the canary here")
	case v.Fallback:
		report(warn, "volume "+safeLabel(name),
			"no directory you own on this filesystem, so backups go to the session store as "+
				"size-capped copies. Creating a directory of your own on this volume fixes it")
	default:
		// An unrecognised token is printed verbatim rather than guessed at:
		// a future save method that costs nothing would otherwise be described
		// as expensive on the strength of not being "link".
		free := "saved by an unrecognised method"
		switch v.Method {
		case "link":
			free = "hardlinked, costing nothing"
		case "copy":
			free = "copied, costing real bytes"
		}
		report(pass, "volume "+safeLabel(name), fmt.Sprintf("store %s; deletions %s (%s)",
			safeLabel(v.StoreRoot), free, v.Method))
	}
	if v.ReflinkKnown {
		state := "no"
		if v.Reflink {
			state = "yes"
		}
		fmt.Printf("       reflink on this filesystem: %s (not yet used by the shim)\n", state)
	} else if v.ReflinkErr != "" {
		fmt.Printf("       reflink on this filesystem: could not tell (%s)\n",
			safeLabel(v.ReflinkErr))
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
