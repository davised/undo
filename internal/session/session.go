// Package session locates and manages per-command session directories.
//
// Layout: $UNDO_DATA_DIR/sessions/<id>/
//
//	cmd      - the command line that ran
//	journal  - shim journal (absent if the command changed nothing)
//	data/    - file backups
//	undone   - marker written after a successful undo
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edaywalid/undo/internal/journal"
)

type Session struct {
	ID      string
	Dir     string
	Cmd     string
	Undone  bool
	Entries []journal.Entry
}

// Root returns the sessions directory, honoring UNDO_DATA_DIR.
func Root() string {
	if d := os.Getenv("UNDO_DATA_DIR"); d != "" {
		return filepath.Join(d, "sessions")
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "undo", "sessions")
}

func load(dir string) (*Session, error) {
	s := &Session{ID: filepath.Base(dir), Dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, "cmd")); err == nil {
		s.Cmd = strings.TrimSpace(string(b))
	}
	if _, err := os.Stat(filepath.Join(dir, "undone")); err == nil {
		s.Undone = true
	}
	entries, err := journal.Read(filepath.Join(dir, "journal"))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	s.Entries = entries
	return s, nil
}

// List returns all sessions, newest first. Session IDs are
// lexicographically sortable timestamps, so a name sort is a time sort.
func List() ([]*Session, error) {
	dirs, err := os.ReadDir(Root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	sessions := make([]*Session, 0, len(names))
	for _, n := range names {
		s, err := load(filepath.Join(Root(), n))
		if err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// Get finds a session by ID, allowing unique prefixes.
func Get(id string) (*Session, error) {
	if s, err := load(filepath.Join(Root(), id)); err == nil && s.Cmd != "" {
		return s, nil
	}
	all, err := List()
	if err != nil {
		return nil, err
	}
	var match *Session
	for _, s := range all {
		if strings.HasPrefix(s.ID, id) {
			if match != nil {
				return nil, os.ErrNotExist
			}
			match = s
		}
	}
	if match == nil {
		return nil, os.ErrNotExist
	}
	return match, nil
}

// Latest returns the most recent session that has changes and has not
// been undone yet.
func Latest() (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if len(s.Entries) > 0 && !s.Undone {
			return s, nil
		}
	}
	return nil, os.ErrNotExist
}

// LatestUndone returns the most recent session in the undone state.
func LatestUndone() (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if len(s.Entries) > 0 && s.Undone {
			return s, nil
		}
	}
	return nil, os.ErrNotExist
}

// Create makes a fresh session directory for cmd, ready for the shim.
func Create(cmd string) (*Session, error) {
	id := fmt.Sprintf("%d%06d", time.Now().Unix(), time.Now().Nanosecond()/1000)
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd"), []byte(cmd+"\n"), 0o644); err != nil {
		return nil, err
	}
	return &Session{ID: id, Dir: dir, Cmd: cmd}, nil
}

// Remove deletes a session and its backups entirely.
func (s *Session) Remove() error {
	return os.RemoveAll(s.Dir)
}

// MarkUndone records that a session was reverted.
func (s *Session) MarkUndone() error {
	return os.WriteFile(filepath.Join(s.Dir, "undone"), nil, 0o644)
}

// ClearUndone flips a session back to the applied state after a redo.
func (s *Session) ClearUndone() error {
	err := os.Remove(filepath.Join(s.Dir, "undone"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
