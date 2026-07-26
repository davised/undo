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
func cmdDoctor() {
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
		reportRoundTrip(report, shim)
	}

	fmt.Println()
	switch worst {
	case pass:
		fmt.Println("all good. try:  touch x && rm x && undo")
	case warn:
		fmt.Println("usable, with warnings above.")
	default:
		fmt.Println("not working yet. fix the FAIL lines above.")
		os.Exit(1)
	}
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
	probe := filepath.Join(root, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		report(failed, "store", "not writable: "+err.Error())
		return
	}
	os.Remove(probe)

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

func reportHooks(report func(checkState, string, string), shim string) {
	// hooks install next to the shim, under share/undo/
	if shim == "" {
		return
	}
	// .../lib/undo/libundo.so -> .../share/undo/
	base := filepath.Dir(filepath.Dir(filepath.Dir(shim)))
	shareDir := filepath.Join(base, "share", "undo")
	var found []string
	for _, h := range []string{"undo.zsh", "undo.bash", "undo.fish"} {
		if _, err := os.Stat(filepath.Join(shareDir, h)); err == nil {
			found = append(found, strings.TrimPrefix(h, "undo."))
		}
	}
	if len(found) == 0 {
		report(warn, "hooks", "not found near the shim; source one in your shell rc")
		return
	}
	// is the current process descended from a hooked shell? the hook exports
	// nothing persistent, so we can only confirm the files exist and remind.
	report(pass, "hooks", strings.Join(found, ", ")+" available in "+shareDir)
}

func reportRoundTrip(report func(checkState, string, string), shim string) {
	dir, err := os.MkdirTemp("", "undo-doctor-")
	if err != nil {
		report(failed, "round trip", err.Error())
		return
	}
	defer os.RemoveAll(dir)

	victim := filepath.Join(dir, "canary.txt")
	const body = "undo doctor canary\n"
	if err := os.WriteFile(victim, []byte(body), 0o644); err != nil {
		report(failed, "round trip", err.Error())
		return
	}

	sess, err := session.Create("undo doctor self-test")
	if err != nil {
		report(failed, "round trip", err.Error())
		return
	}
	defer sess.Remove()

	// delete the canary through a shell with the shim armed
	cmd := exec.Command("/bin/sh", "-c", "rm "+victim)
	cmd.Env = armedEnv(os.Environ(), shim, sess.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		report(failed, "capture", fmt.Sprintf("rm failed: %v %s", err, out))
		return
	}
	sess.MarkDone()

	entries, _ := journal.Read(filepath.Join(sess.Dir, "journal"))
	if len(entries) == 0 {
		report(failed, "capture",
			"the shim did not record the deletion (LD_PRELOAD may be blocked here)")
		return
	}
	report(pass, "capture", fmt.Sprintf("%d change recorded", len(entries)))

	// reload and restore
	fresh, err := session.Get(sess.ID)
	if err != nil {
		report(failed, "restore", err.Error())
		return
	}
	if _, err := restore.Run(fresh, restore.Undo, restore.Options{}); err != nil {
		report(failed, "restore", err.Error())
		return
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != body {
		report(failed, "restore", "canary not restored to its original contents")
		return
	}
	report(pass, "restore", "canary recovered intact")
}
