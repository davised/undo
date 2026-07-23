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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edaywalid/undo/internal/journal"
)

type Session struct {
	ID      string
	Dir     string
	Cmd     string
	Undone  bool
	Done    bool // the command finished (done marker present)
	Pid     int  // shell or runner pid, 0 for pre-lock sessions
	Entries []journal.Entry
}

// Live reports whether the session's command may still be running.
// Sessions without a pid file (old format) are assumed finished.
func (s *Session) Live() bool {
	if s.Done || s.Pid <= 0 {
		return false
	}
	err := syscall.Kill(s.Pid, 0)
	return err == nil || err == syscall.EPERM
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
	if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
		s.Done = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
		s.Pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
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
	if err := os.MkdirAll(Root(), 0o700); err != nil {
		return nil, err
	}
	TightenPerms()
	var id, dir string
	for {
		now := time.Now()
		id = fmt.Sprintf("%d%06d", now.Unix(), now.Nanosecond()/1000)
		dir = filepath.Join(Root(), id)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return nil, err
		}
		time.Sleep(time.Microsecond) // same-microsecond collision
	}
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "cmd"), []byte(cmd+"\n"), 0o600); err != nil {
		return nil, err
	}
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(pid+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &Session{ID: id, Dir: dir, Cmd: cmd, Pid: os.Getpid()}, nil
}

// MarkDone records that the session's command finished.
func (s *Session) MarkDone() error {
	return os.WriteFile(filepath.Join(s.Dir, "done"), nil, 0o600)
}

// TightenPerms keeps the store private: backups may contain copies of
// sensitive files.
func TightenPerms() {
	os.Chmod(filepath.Dir(Root()), 0o700)
	os.Chmod(Root(), 0o700)
}

func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// GC removes empty sessions and prunes the oldest until at most keep
// sessions remain within maxBytes total. Live sessions are never touched.
func GC(keep int, maxBytes int64) (int, error) {
	TightenPerms()
	all, err := List()
	if err != nil {
		return 0, err
	}
	removed, kept := 0, 0
	var total int64
	for _, s := range all { // newest first
		if s.Live() {
			continue
		}
		if len(s.Entries) == 0 {
			if s.Remove() == nil {
				removed++
			}
			continue
		}
		kept++
		total += dirSize(s.Dir)
		if kept > keep || total > maxBytes {
			if s.Remove() == nil {
				removed++
			}
		}
	}
	return removed, nil
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
