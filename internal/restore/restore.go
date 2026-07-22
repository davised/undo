// Package restore replays a session journal in reverse.
package restore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/edaywalid/undo/internal/journal"
	"github.com/edaywalid/undo/internal/session"
)

type Options struct {
	DryRun bool
	Force  bool // clobber files that changed since the session
}

type Result struct {
	Done    int
	Skipped []string
	Actions []string // populated on dry runs
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// moveFile renames backup into place, copying across filesystems if the
// session store lives on a different device than the restored path.
func moveFile(backup, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(backup, dst); err == nil {
		return nil
	}
	in, err := os.Open(backup)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(backup)
}

// Run undoes every journal entry of s, newest first.
func Run(s *session.Session, opts Options) (*Result, error) {
	res := &Result{}
	skip := func(e journal.Entry, why string) {
		res.Skipped = append(res.Skipped, e.Describe()+": "+why)
	}
	act := func(e journal.Entry) bool {
		if opts.DryRun {
			res.Actions = append(res.Actions, "would revert: "+e.Describe())
			return false
		}
		return true
	}

	for i := len(s.Entries) - 1; i >= 0; i-- {
		e := s.Entries[i]
		f := e.Fields
		field := func(n int) string {
			if n < len(f) {
				return f[n]
			}
			return ""
		}

		var err error
		switch e.Op {
		case journal.OpCreate:
			if !exists(field(0)) {
				continue
			}
			if !act(e) {
				continue
			}
			err = os.Remove(field(0))

		case journal.OpUnlink:
			if exists(field(0)) && !opts.Force {
				skip(e, "path exists again, use --force to overwrite")
				continue
			}
			if !act(e) {
				continue
			}
			err = moveFile(field(1), field(0))

		case journal.OpRmlink:
			if exists(field(0)) && !opts.Force {
				skip(e, "path exists again, use --force to overwrite")
				continue
			}
			if !act(e) {
				continue
			}
			os.Remove(field(0))
			err = os.Symlink(field(1), field(0))

		case journal.OpMod:
			if !act(e) {
				continue
			}
			err = moveFile(field(1), field(0))

		case journal.OpRename:
			if exists(field(0)) && !opts.Force {
				skip(e, "original path occupied, use --force to overwrite")
				continue
			}
			if !exists(field(1)) {
				skip(e, "moved file is gone")
				continue
			}
			if !act(e) {
				continue
			}
			os.Remove(field(0))
			if err = os.Rename(field(1), field(0)); err == nil && field(2) != "-" && field(2) != "" {
				err = moveFile(field(2), field(1))
			}

		case journal.OpExchange:
			if !exists(field(0)) || !exists(field(1)) {
				skip(e, "one side is gone")
				continue
			}
			if !act(e) {
				continue
			}
			tmp := field(0) + ".undo-xchg"
			if err = os.Rename(field(0), tmp); err == nil {
				if err = os.Rename(field(1), field(0)); err == nil {
					err = os.Rename(tmp, field(1))
				}
			}

		case journal.OpMkdir:
			if !exists(field(0)) {
				continue
			}
			if !act(e) {
				continue
			}
			if opts.Force {
				err = os.RemoveAll(field(0))
			} else if err = os.Remove(field(0)); err != nil {
				skip(e, "directory not empty, use --force to delete it")
				continue
			}

		case journal.OpRmdir:
			if exists(field(0)) {
				continue
			}
			if !act(e) {
				continue
			}
			mode, perr := strconv.ParseUint(field(1), 8, 32)
			if perr != nil {
				mode = 0o755
			}
			if err = os.MkdirAll(field(0), os.FileMode(mode)); err == nil {
				err = os.Chmod(field(0), os.FileMode(mode))
			}

		case journal.OpLost:
			skip(e, "the shim could not save a backup (too big or unlinkable)")
			continue

		default:
			skip(e, "unknown journal op")
			continue
		}

		if err != nil {
			skip(e, err.Error())
			continue
		}
		if !opts.DryRun {
			res.Done++
		}
	}

	if !opts.DryRun && res.Done > 0 {
		if err := s.MarkUndone(); err != nil {
			return res, fmt.Errorf("restored, but could not mark session undone: %w", err)
		}
	}
	return res, nil
}
