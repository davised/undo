package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/edaywalid/undo/internal/session"
)

// findShim locates libundo.so: env override, install locations relative
// to the binary, then the usual system paths.
func findShim() string {
	if p := os.Getenv("UNDO_LIB"); p != "" {
		return p
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "..", "lib", "undo", "libundo.so"),
			filepath.Join(dir, "..", "build", "libundo.so"), // dev tree
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "lib", "undo", "libundo.so"))
	}
	candidates = append(candidates,
		"/usr/local/lib/undo/libundo.so", "/usr/lib/undo/libundo.so")
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// cmdRun executes one command with the shim armed, no shell hook needed.
func cmdRun(argv []string) {
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fatal(fmt.Errorf("run needs a command: undo run -- <cmd> [args...]"))
	}
	shim := findShim()
	if shim == "" {
		fatal(fmt.Errorf("libundo.so not found; set UNDO_LIB or make install"))
	}

	s, err := session.Create(strings.Join(argv, " "))
	if err != nil {
		fatal(err)
	}

	preload := shim
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LD_PRELOAD=") {
			if v := strings.TrimPrefix(kv, "LD_PRELOAD="); v != "" {
				preload = shim + ":" + v
			}
			continue
		}
		if strings.HasPrefix(kv, "UNDO_SESSION=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "UNDO_SESSION="+s.Dir, "LD_PRELOAD="+preload)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env

	runErr := cmd.Run()

	code := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			s.Remove()
			fatal(runErr)
		}
	}

	// reload to see what the shim recorded
	if fresh, err := session.Get(s.ID); err == nil && len(fresh.Entries) > 0 {
		fmt.Fprintf(os.Stderr, "undo: captured %d change(s), run 'undo' to revert\n",
			len(fresh.Entries))
	} else {
		s.Remove()
	}
	os.Exit(code)
}
