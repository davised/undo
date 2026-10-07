package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
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

// The defaults the consumers fall back to. defaultMaxBytes matches
// DEFAULT_MAX_BYTES in shim/undo_shim.c; defaultMaxStore matches the 1 GiB
// envInt fallback for UNDO_MAX_STORE.
const (
	defaultMaxBytes = 256 << 20
	defaultMaxStore = 1 << 30
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

	// The limits are global, so a misread one is reported once, not per volume.
	reportLimitMisreads(report)

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
		// The method is mapped exactly as in the default branch below: a
		// fallback onto the same filesystem can still be hardlinked, so
		// assuming "size-capped copy" here would describe a cost the journal
		// does not show.
		free := "saved by an unrecognised method"
		switch v.Method {
		case "link":
			free = "hardlinked, costing nothing"
		case "copy":
			free = "copied, costing real bytes"
		}
		report(warn, "volume "+safeLabel(name), fmt.Sprintf(
			"backups go to the session store (%s, %s): undo could not use a store on "+
				"this filesystem -- usually no directory you own here, or a .undo in the "+
				"way that is not a directory you own. Creating a directory of your own "+
				"on this volume usually fixes it", free, v.Method))
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
		humanBytes(effectiveMaxBytes(os.Getenv("UNDO_MAX_BYTES"))),
		humanBytes(uint64(effectiveMaxStore(os.Getenv("UNDO_MAX_STORE")))))
}

// reportLimitMisreads warns when an environment value would not be read the way
// a worded unit (or a sign, or trailing junk) suggests. It never changes what
// the consumers do -- it only stops doctor from printing a cap the shim and gc
// do not enforce.
func reportLimitMisreads(report func(checkState, string, string)) {
	if raw := os.Getenv("UNDO_MAX_BYTES"); raw != "" && maxBytesMisread(raw) {
		report(warn, "limits", fmt.Sprintf(
			"UNDO_MAX_BYTES=\"%s\" is read as %s; give a plain number of bytes",
			safeLabel(raw), humanBytes(effectiveMaxBytes(raw))))
	}
	if raw := os.Getenv("UNDO_MAX_STORE"); raw != "" && maxStoreMisread(raw) {
		report(warn, "limits", fmt.Sprintf(
			"UNDO_MAX_STORE=\"%s\" is read as %s; give a plain number of bytes",
			safeLabel(raw), humanBytes(uint64(effectiveMaxStore(raw)))))
	}
}

// effectiveMaxBytes mirrors the shim's max_bytes(): parse_ulong over
// UNDO_MAX_BYTES, then the zero result falls back to the 256 MiB default.
// UNDO_MAX_BYTES="256MiB" therefore means 256 BYTES, not 256 MiB.
func effectiveMaxBytes(raw string) uint64 {
	if v := parseULong(raw); v != 0 {
		return v
	}
	return defaultMaxBytes
}

// effectiveMaxStore is what envInt derives from UNDO_MAX_STORE: a base-10
// integer that fails to parse or is <= 0 silently becomes the 1 GiB default.
func effectiveMaxStore(raw string) int64 {
	return parsePositiveInt(raw, defaultMaxStore)
}

// maxBytesMisread reports whether UNDO_MAX_BYTES(raw) is read differently from
// how it is written: anything but a leading run of decimal digits after
// optional leading spaces/tabs, or a value that still parses to zero.
func maxBytesMisread(raw string) bool {
	s := strings.TrimLeft(raw, " \t")
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return true
		}
	}
	return parseULong(s) == 0
}

// maxStoreMisread reports whether envInt would reject the raw value: not a
// base-10 integer, or <= 0.
func maxStoreMisread(raw string) bool {
	n, err := strconv.ParseInt(raw, 10, 64)
	return err != nil || n <= 0
}

// parseULong mirrors parse_ulong in shim/undo_shim.c: skip leading spaces and
// tabs, consume the leading run of decimal digits, stop at the first other
// byte, and saturate at the unsigned maximum instead of wrapping. An
// unparseable value is zero, which max_bytes then replaces with its default.
func parseULong(s string) uint64 {
	var v uint64
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		d := uint64(s[i] - '0')
		if v > (math.MaxUint64-d)/10 {
			return math.MaxUint64
		}
		v = v*10 + d
	}
	return v
}

// humanBytes renders a byte count in the largest binary unit that keeps it at
// least 1, with up to one decimal place.
func humanBytes(n uint64) string {
	units := []struct {
		name string
		size uint64
	}{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
	}
	for _, u := range units {
		if n >= u.size {
			q, r := n/u.size, n%u.size
			tenths := (r * 10) / u.size
			if tenths == 0 {
				return fmt.Sprintf("%d %s", q, u.name)
			}
			return fmt.Sprintf("%d.%d %s", q, tenths, u.name)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func parsePositiveInt(raw string, def int64) int64 {
	if raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n
		}
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
