package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const installURL = "https://undo.edaywalid.com/install.sh"

// installKind is how this copy of undo got onto the machine. Upgrading a
// package-managed copy behind the package manager's back leaves it with a
// stale database and files it does not own, so we only self-upgrade the
// copies we installed ourselves.
type installKind int

const (
	installSelf installKind = iota // curl installer or make install
	installBrew
	installSystem // dpkg / rpm / pacman
)

func detectInstall(exe string) (installKind, string) {
	switch {
	case strings.Contains(exe, "/homebrew/"), strings.Contains(exe, "/linuxbrew/"):
		return installBrew, "brew upgrade undo"
	case strings.HasPrefix(exe, "/usr/"):
		// whichever of these exists on the box
		for _, c := range [][2]string{
			{"pacman", "sudo pacman -Syu undo-cli-bin"},
			{"dpkg", "download the new .deb from the releases page, then sudo dpkg -i undo_*.deb"},
			{"rpm", "download the new .rpm from the releases page, then sudo rpm -U undo_*.rpm"},
		} {
			if _, err := exec.LookPath(c[0]); err == nil {
				return installSystem, c[1]
			}
		}
		return installSystem, "use your package manager"
	default:
		return installSelf, ""
	}
}

// latestTag asks GitHub for the newest release tag.
func latestTag() (string, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/edaywalid/undo/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no tag in the release response")
	}
	return rel.TagName, nil
}

func sameVersion(current, tag string) bool {
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(tag, "v")
}

// cmdUpgrade updates a self-installed undo in place, or explains how to
// upgrade a package-managed one.
func cmdUpgrade(checkOnly bool) {
	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	fmt.Printf("installed: %s (%s)\n", version, exe)
	tag, err := latestTag()
	if err != nil {
		fatal(fmt.Errorf("could not check for updates: %w", err))
	}
	fmt.Printf("latest:    %s\n", tag)

	if sameVersion(version, tag) {
		fmt.Println("\nalready up to date.")
		return
	}
	if version == "dev" {
		fmt.Println("\nthis is a dev build; upgrade would replace it with a release.")
	}
	if checkOnly {
		fmt.Printf("\n%s is available. run 'undo upgrade' to install it.\n", tag)
		return
	}

	kind, how := detectInstall(exe)
	if kind != installSelf {
		fmt.Printf("\nthis copy came from a package manager, so upgrade it with:\n  %s\n", how)
		os.Exit(1)
	}

	// prefix is .../bin/undo -> ...
	prefix := filepath.Dir(filepath.Dir(exe))
	fmt.Printf("\nupgrading %s -> %s in %s\n\n", version, tag, prefix)

	script, err := fetch(installURL)
	if err != nil {
		fatal(fmt.Errorf("could not fetch the installer: %w", err))
	}
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PREFIX="+prefix)
	if err := cmd.Run(); err != nil {
		fatal(fmt.Errorf("upgrade failed: %w", err))
	}
	fmt.Println("\nopen a new shell to pick up the updated hook.")
}

func fetch(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
