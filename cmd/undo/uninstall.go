package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edaywalid/undo/internal/session"
)

// rcFiles are the shell rc files the installer may have written to.
func rcFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
}

// installedPaths lists what a self-install put on disk, given its prefix.
// The session store is deliberately NOT here: it holds the user's backups.
func installedPaths(prefix string) []string {
	return []string{
		filepath.Join(prefix, "bin", "undo"),
		filepath.Join(prefix, "lib", "undo"),
		filepath.Join(prefix, "share", "undo", "undo.zsh"),
		filepath.Join(prefix, "share", "undo", "undo.bash"),
		filepath.Join(prefix, "share", "undo", "undo.fish"),
		filepath.Join(prefix, "share", "zsh", "site-functions", "_undo"),
		filepath.Join(prefix, "share", "bash-completion", "completions", "undo"),
		filepath.Join(prefix, "share", "fish", "vendor_completions.d", "undo.fish"),
	}
}

// stripHookLines removes the block the installer appended: our marker
// comment, the source line, and the PATH line that came with it. Returns
// the cleaned content and how many lines went.
func stripHookLines(content string) (string, int) {
	const marker = "# undo: revert what the last command did"
	isSourceLine := func(t string) bool {
		return strings.HasPrefix(t, "source ") && strings.Contains(t, "/undo/undo.")
	}
	// Only ever removed as part of our block: a PATH line points at
	// ~/.local/bin, which has nothing identifying in it, so matching one by
	// content alone would risk deleting a line we never wrote.
	isPathLine := func(t string) bool {
		return strings.HasPrefix(t, "export PATH=") || strings.HasPrefix(t, "fish_add_path ")
	}

	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	removed := 0
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])

		if strings.HasPrefix(t, marker) {
			// drop the blank line the installer put above the block
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			removed++
			// then the lines belonging to it
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if !isSourceLine(next) && !isPathLine(next) {
					break
				}
				i++
				removed++
			}
			continue
		}

		// a source line added by hand, without our marker
		if isSourceLine(t) {
			removed++
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n"), removed
}

func cmdUninstall(yes, purge bool) {
	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	kind, how := detectInstall(exe)
	if kind != installSelf {
		fmt.Printf("this copy came from a package manager, so remove it with:\n  %s\n",
			strings.Replace(strings.Replace(how, "upgrade", "uninstall", 1),
				"-Syu", "-Rns", 1))
		fmt.Println("\nthen run 'undo uninstall' again to clean the shell rc lines,")
		fmt.Println("or delete them by hand.")
		os.Exit(1)
	}

	prefix := filepath.Dir(filepath.Dir(exe))
	var present []string
	for _, p := range installedPaths(prefix) {
		if _, err := os.Lstat(p); err == nil {
			present = append(present, p)
		}
	}

	store := session.Root()
	fmt.Println("this will remove:")
	for _, p := range present {
		fmt.Println("  " + p)
	}
	for _, rc := range rcFiles() {
		b, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		if _, n := stripHookLines(string(b)); n > 0 {
			fmt.Printf("  the undo lines in %s\n", rc)
		}
	}
	if purge {
		fmt.Println("  " + store + "  (every stored backup)")
	} else {
		fmt.Println("\nkeeping your session store at " + store)
		fmt.Println("(pass --purge to delete the backups too)")
	}

	if !yes && !confirm("\nremove undo? [y/N] ") {
		fmt.Println("aborted")
		return
	}

	failures := 0
	for _, p := range present {
		if err := os.RemoveAll(p); err != nil {
			fmt.Fprintln(os.Stderr, "could not remove", p, err)
			failures++
		}
	}
	// share/undo may now be empty; take it if so, but never if the store
	// lives inside it
	shareUndo := filepath.Join(prefix, "share", "undo")
	if !strings.HasPrefix(store, shareUndo+string(os.PathSeparator)) {
		os.Remove(shareUndo)
	}

	for _, rc := range rcFiles() {
		b, err := os.ReadFile(rc)
		if err != nil {
			continue
		}
		cleaned, n := stripHookLines(string(b))
		if n == 0 {
			continue
		}
		info, err := os.Stat(rc)
		mode := os.FileMode(0o644)
		if err == nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(rc, []byte(cleaned), mode); err != nil {
			fmt.Fprintln(os.Stderr, "could not clean", rc, err)
			failures++
			continue
		}
		fmt.Printf("cleaned %s\n", rc)
	}

	if purge {
		if err := os.RemoveAll(store); err != nil {
			fmt.Fprintln(os.Stderr, "could not remove the store:", err)
			failures++
		}
	}

	if failures > 0 {
		fatal(fmt.Errorf("%d item(s) could not be removed", failures))
	}
	fmt.Println("\nundo removed. open a new shell to drop the hook from this session.")
	if !purge {
		fmt.Println("your backups are still in " + store)
	}
}
