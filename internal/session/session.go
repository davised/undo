// Package session locates and manages per-command session directories.
//
// Layout: $UNDO_DATA_DIR/sessions/<id>/
//
//	cmd      - the command line that ran
//	journal  - shim journal (absent if the command changed nothing)
//	data/    - file backups
//	undone   - marker written after a successful undo, holding the RFC3339
//	           nanosecond time of the undo so redo can find the session that
//	           was undone last rather than the one that ran last
//	pid      - the pid of the shell or runner that started the command
//	host     - the scope in which the pid above means something: hostname,
//	           boot id and pid namespace, tab separated. Homes are shared
//	           across nodes, so a pid alone does not say whether the command
//	           is still running; see Live.
package session

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edaywalid/undo/internal/journal"
)

type Session struct {
	ID       string
	Dir      string
	Cmd      string
	Undone   bool
	UndoneAt time.Time // when the undo happened, zero if not undone
	Done     bool      // the command finished (done marker present)
	Pid      int       // shell or runner pid, 0 for pre-lock sessions
	Origin   string    // where its pid is meaningful, empty for older sessions
	Entries  []journal.Entry
}

// Live reports whether the session's command may still be running.
//
// A pid is only meaningful within the kernel instance and namespace that
// issued it, and homes are shared across nodes, so the local probe is trusted
// only when the origin matches. A session from elsewhere cannot be probed at
// all: it is treated as running until it outlives foreignGrace, because
// deleting a running command's backups destroys data while keeping a finished
// session merely wastes space.
//
// Sessions written before origins were recorded have none, and keep the old
// pid-probe behaviour so a rollout does not strand what is already on disk.
func (s *Session) Live() bool {
	if s.Done {
		return false
	}
	// The pid is not consulted for a session from elsewhere -- not even to
	// notice it is missing. load turns an absent, truncated or unparsable pid
	// file into zero, and on a store shared between nodes that is an ordinary
	// sight rather than an error: a session whose creator is still writing it.
	// Checking Pid <= 0 first would call those finished and collect them.
	//
	// An empty local identity means we could not establish which kernel
	// instance we are, so nothing can be classified. Neither signal is sound
	// alone there -- the grace alone would let a local running command be
	// purged once it aged out, and the probe alone would collect a foreign one
	// -- so a session is finished only when both agree it is.
	local := thisHost()
	if local == "" {
		return s.probe() || s.withinGrace()
	}
	if s.Origin != "" && s.Origin != local {
		return s.withinGrace()
	}
	return s.probe()
}

// probe asks the kernel whether this session's pid is still around. Only
// meaningful for a session issued by this kernel instance.
func (s *Session) probe() bool {
	if s.Pid <= 0 {
		return false
	}
	err := syscall.Kill(s.Pid, 0)
	return err == nil || err == syscall.EPERM
}

// withinGrace reports whether a session from another kernel instance is young
// enough to still be presumed running.
func (s *Session) withinGrace() bool {
	started := s.Started()
	if started.IsZero() {
		return true // unreadable id: nothing to age it out by, so do not
	}
	return time.Since(started) < foreignGrace()
}

// minForeignGrace floors the grace period.
//
// The age of a foreign session is its id -- the creating node's wall clock --
// measured against ours. Skew between the two is therefore an error in the
// age, and a grace shorter than the skew would collect a command that started
// moments ago. Clusters keep clocks synchronised to well inside this floor;
// the floor is what stops a mistuned grace from turning ordinary skew into
// data loss.
const minForeignGrace = 15 * time.Minute

// maxGraceSeconds is the largest grace that fits in a time.Duration, which
// counts nanoseconds in an int64 -- about 292 years.
//
// Anything past it must saturate rather than wrap. A wrapped value is
// negative, so it falls under the floor and silently becomes 15 minutes: the
// exact opposite of what someone setting an enormous number is asking for,
// and it would collect running commands after a quarter of an hour.
const maxGraceSeconds = int64(1<<63-1) / int64(time.Second)

// foreignGrace is how long a session from another node is presumed to be
// running when it has no done marker.
//
// Normal termination writes that marker, so this only has to cover abnormal
// ends -- a killed job, a crashed node -- which makes a generous default
// cheap. The bound is real: a command running longer than this becomes
// collectible while still running. Raise it where jobs outlive it.
func foreignGrace() time.Duration {
	secs := envSeconds("UNDO_FOREIGN_GRACE", 7*24*60*60)
	if secs >= maxGraceSeconds {
		return time.Duration(1<<63 - 1)
	}
	g := time.Duration(secs) * time.Second
	if g < minForeignGrace {
		return minForeignGrace
	}
	return g
}

// envSeconds reads a positive integer from the environment, or returns def.
func envSeconds(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

var (
	hostOnce sync.Once
	hostID   string
)

// thisHost identifies the scope in which this session's pid means something:
// hostname, boot id, and pid namespace.
//
// If any part cannot be read the result is empty, which Live treats as
// "cannot compare" and resolves explicitly.
func thisHost() string {
	hostOnce.Do(func() {
		name, _ := os.Hostname()
		var boot string
		if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
			boot = strings.TrimSpace(string(b))
		}
		// "pid:[4026532537]" -- the namespace's inode, distinct per namespace
		pidns, _ := os.Readlink("/proc/self/ns/pid")
		hostID = composeHost(name, boot, strings.TrimSpace(pidns))
	})
	return hostID
}

// composeHost joins the three parts of a host identity, or returns empty if
// any is missing.
//
// All or nothing, because each rules out a different way for a pid to be
// misread. Without the boot id, a session from before a reboot looks local and
// its pid may since have been reissued. Without the pid namespace, containers
// that share a kernel and a UTS namespace -- several in one pod, say -- look
// identical while the same number names unrelated processes in each. The
// hostname is what makes the other two legible to a human reading the file.
//
// A subset is not a weaker version of this; it is a different and wrong answer.
func composeHost(name, boot, pidns string) string {
	if name == "" || boot == "" || pidns == "" {
		return ""
	}
	return name + "\t" + boot + "\t" + pidns
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
	if fi, err := os.Stat(filepath.Join(dir, "undone")); err == nil {
		s.Undone = true
		s.UndoneAt = fi.ModTime()
		// Markers written before undo recorded a time are empty; their
		// mtime above is the fallback, and it says the same thing.
		if b, err := os.ReadFile(filepath.Join(dir, "undone")); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err == nil {
				s.UndoneAt = t
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
		s.Done = true
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
		s.Pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if b, err := os.ReadFile(filepath.Join(dir, "host")); err == nil {
		s.Origin = strings.TrimSpace(string(b))
	}
	entries, err := journal.Read(filepath.Join(dir, "journal"))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	s.Entries = journal.ResolveStoreMoves(entries)
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

// LatestUndone returns the session that was undone most recently, which is
// the one a bare `undo redo` should re-apply.
//
// Not the newest undone session by command time: after undoing two commands
// in a row, the second undo targets the older command, so the session you
// just undid is the older one. Picking by command time would re-apply the
// wrong session and leave the one you actually undid still reverted.
func LatestUndone() (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var best *Session
	for _, s := range all {
		if len(s.Entries) == 0 || !s.Undone {
			continue
		}
		// all is newest-command-first, so a strict > keeps that as the
		// tiebreak when two markers share a timestamp
		if best == nil || s.UndoneAt.After(best.UndoneAt) {
			best = s
		}
	}
	if best == nil {
		return nil, os.ErrNotExist
	}
	return best, nil
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
	// Written as its own file rather than folded into pid: during a rollout
	// the binary and the shell hooks update at different times, and an older
	// undo reading a newer session must not trip over a changed pid format.
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(thisHost()+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &Session{ID: id, Dir: dir, Cmd: cmd, Pid: os.Getpid(), Origin: thisHost()}, nil
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

// storeDirName is the per-filesystem store directory the shim creates.
const storeDirName = ".undo"

// rootsFile is the registry of store roots, kept beside the sessions
// directory so it outlives any individual session.
func rootsFile() string {
	return filepath.Join(filepath.Dir(Root()), "roots")
}

// sessionIDLen is the width Create produces: fmt.Sprintf("%d%06d", unix,
// nanos/1000) -- ten digits of unix seconds through the year 2286, then six of
// microseconds.
const sessionIDLen = 16

// sessionID reports whether name has exactly the shape Create produces.
//
// The sweep calls os.RemoveAll on whatever this approves, so it is a deletion
// fence, not a formatting detail. Anything looser -- "all digits", "not a
// dotfile" -- turns a shared .undo directory into a hazard, because a numeric
// directory belonging to something else would qualify.
//
// Being too strict fails closed: an orphan survives and is reclaimed by hand.
// Being too loose deletes someone else's data. If Create's format ever
// changes, update this and accept that older orphans stop being swept.
func sessionID(name string) bool {
	if len(name) != sessionIDLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
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

// allocatedBytes is what this session actually costs in space: the session
// directory, plus every backup the journal names whose save method allocated
// something.
//
// A hardlink is a second name for blocks that already exist, so it allocates
// nothing and is excluded. Counting one at full logical size -- which is what
// walking the directory does -- means a hardlinked 50 GiB deletion is charged
// 50 GiB against a 1 GiB budget and evicted on the next command, making the
// free mechanism the first one pruned. Hardlinks are bounded by session count
// and UNDO_MAX_AGE instead, since their cost is deferred reclamation.
//
// Only non-hardlinked backups are stat'd, which is also what keeps this cheap:
// a command deleting 100k files produces 100k hardlink records and zero round
// trips here, while copies are capped at UNDO_MAX_BYTES each and far rarer.
func (s *Session) allocatedBytes() int64 {
	return dirSize(s.Dir) + chargeTotal(s.distributedCharges())
}

// backupCharge is one backup held outside the session directory, and what it
// costs. The size is captured up front because it cannot be recovered later: a
// backup on a volume that has gone away still occupies space, and stat will
// not say how much.
type backupCharge struct {
	path string
	size int64
}

func chargeTotal(cs []backupCharge) int64 {
	var total int64
	for _, c := range cs {
		total += c.size
	}
	return total
}

// distributedCharges lists this session's backups outside its own directory.
// Hardlinks are excluded for the reason above.
func (s *Session) distributedCharges() []backupCharge {
	var out []backupCharge
	prefix := s.Dir + string(os.PathSeparator)
	for _, e := range s.Entries {
		if m := e.Method(); m == "link" || m == "none" {
			continue
		}
		b := e.Backup()
		if b == "" || b == "-" || strings.HasPrefix(b, prefix) {
			continue // no backup, discarded, or already counted by dirSize
		}
		if fi, err := os.Lstat(b); err == nil && fi.Mode().IsRegular() {
			out = append(out, backupCharge{b, fi.Size()})
		}
	}
	return out
}

// GC removes empty sessions and prunes the oldest until at most keep
// sessions remain within maxBytes of allocated space. Sessions older than
// maxAge go regardless; maxAge of 0 disables that. Live sessions are never
// touched.
func GC(keep int, maxBytes int64, maxAge time.Duration) (int, error) {
	TightenPerms()
	all, err := List()
	if err != nil {
		return 0, err
	}

	// Learn the roots before anything is pruned: a session removed below takes
	// its journal, and with it the only record of where its backups lived.
	var roots []string
	for _, s := range all {
		roots = append(roots, storeRoots(s.Entries)...)
	}
	// Before pruning, not after, and fatal if it fails: a pruned session takes
	// its journal, which is the only record of where its backups live. Pruning
	// without that recorded means any backup Remove cannot delete -- an
	// unmounted volume, say -- is unreachable by the sweep forever.
	if err := rememberRoots(roots); err != nil {
		return 0, err
	}

	// seen counts sessions considered, not sessions retained, and is
	// deliberately not decremented on removal: keep means "the newest N", so
	// everything past position N goes regardless of what was freed ahead of it.
	removed, seen := 0, 0
	var total int64
	now := time.Now()
	for _, s := range all { // newest first
		if s.Live() {
			continue
		}
		if len(s.Entries) == 0 {
			// A done marker is conclusive: the command finished and recorded
			// nothing. Without one, an empty session may simply be one whose
			// creator has not written its first record yet, and on a store
			// shared between nodes that setup is not always visible promptly.
			//
			// The bound is foreignGrace and not something shorter because the
			// only clock available here is the creating node's, embedded in
			// the id. A one-minute window judged by a clock a minute behind is
			// no window at all. This is the same bound already accepted for
			// "cannot tell whether it is running", applied to the same doubt.
			if !s.Done && time.Since(s.Started()) < foreignGrace() {
				continue
			}
			if s.Remove() == nil {
				removed++
			}
			continue
		}
		seen++
		dirBytes := dirSize(s.Dir)
		charges := s.distributedCharges()
		total += dirBytes + chargeTotal(charges)

		// The newest session is the one `undo` with no arguments targets,
		// so it always survives. Dropping it because a single big delete
		// blew the size budget would silently remove exactly the undo the
		// user is about to reach for.
		if seen == 1 {
			continue
		}
		tooOld := maxAge > 0 && now.Sub(s.Started()) > maxAge
		if seen > keep || total > maxBytes || tooOld {
			if gone, err := s.removeReporting(); err == nil {
				removed++
				// Give back only what was actually unlinked. RemoveAll took
				// the session directory, so those bytes count. For backups
				// outside it, removeDistributedBackups is best effort, and a
				// path that has stopped resolving is not proof: a filesystem
				// unmounted mid-sweep answers the same way while still holding
				// the bytes. Crediting those would leave the store over budget
				// believing it had made room, so only a successful unlink
				// counts and anything else keeps being charged.
				freed := dirBytes
				for _, c := range charges {
					if gone[c.path] {
						freed += c.size
					}
				}
				total -= freed
			}
		}
	}
	return removed, nil
}

// Started is when the session's command ran, decoded from its id -- which is
// unix seconds followed by six digits of microseconds.
func (s *Session) Started() time.Time {
	if len(s.ID) < 7 {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(s.ID[:len(s.ID)-6], 10, 64)
	if err != nil {
		return time.Time{}
	}
	usec, err := strconv.ParseInt(s.ID[len(s.ID)-6:], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, usec*1000)
}

// backupPaths returns the backup locations this session's journal names.
// The shim records them as absolute paths, so they may be anywhere.
func (s *Session) backupPaths() []string {
	var out []string
	for _, e := range s.Entries {
		p := e.Backup()
		if p == "" || p == "-" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// storeRoots extracts the filesystem-local store roots a journal implies.
// Backups are written to <root>/.undo/<session-id>/<name>, so the root is
// whatever precedes the .undo component.
func storeRoots(entries []journal.Entry) []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range entries {
		p := e.Backup()
		if p == "" || p == "-" || !filepath.IsAbs(p) {
			continue
		}
		// <root>/.undo/<id>/<name> -> <root>
		idDir := filepath.Dir(p)
		undoDir := filepath.Dir(idDir)
		if filepath.Base(undoDir) != storeDirName {
			continue
		}
		root := filepath.Dir(undoDir)
		if root == "" || root == "/" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out
}

// Roots returns the store roots this installation knows about.
func Roots() []string {
	b, err := os.ReadFile(rootsFile())
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !filepath.IsAbs(line) || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// rememberRoots adds roots to the registry, keeping what is already there.
//
// The registry exists because a root has to stay sweepable after the last
// session that used it is pruned -- which is exactly when its orphans become
// unreachable by any other means.
func rememberRoots(roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	all := Roots()
	seen := make(map[string]bool, len(all))
	for _, r := range all {
		seen[r] = true
	}
	changed := false
	for _, r := range roots {
		if !filepath.IsAbs(r) || seen[r] {
			continue
		}
		seen[r] = true
		all = append(all, r)
		changed = true
	}
	if !changed {
		return nil
	}
	sort.Strings(all)
	if err := os.MkdirAll(filepath.Dir(rootsFile()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(rootsFile(), []byte(strings.Join(all, "\n")+"\n"), 0o600)
}

// SweepOrphans removes store directories whose session no longer exists, and
// forgets roots that are reachable and hold nothing.
//
// Deletion here is driven by directory names on disk rather than by a journal,
// so it is fenced three ways: the directory must sit directly under a .undo
// directory beneath a registered root, its name must have the shape of a
// session id, and no session by that name may exist.
//
// A root that cannot be read is kept, not forgotten. An unmounted volume is
// indistinguishable from an empty one from here, and dropping it would strand
// whatever it holds permanently. On node-local filesystems this reclaims only
// on the node that created the store, since no other node can see the path.
func SweepOrphans() (int, error) {
	roots := Roots()
	if len(roots) == 0 {
		return 0, nil
	}
	removed := 0
	var keep []string
	for _, root := range roots {
		undoDir := filepath.Join(root, storeDirName)
		ents, err := os.ReadDir(undoDir)
		if err != nil {
			if os.IsNotExist(err) {
				// Reachable and empty: only then is it safe to forget.
				if _, serr := os.Stat(root); serr == nil {
					continue
				}
			}
			keep = append(keep, root) // unreadable, unmounted, or in use
			continue
		}
		live := 0
		for _, e := range ents {
			cand := filepath.Join(undoDir, e.Name())
			// safeStore, not realDir alone: it also requires the .undo
			// parent to be a real directory. ReadDir follows a symlinked
			// .undo, and the fences on the entries say nothing about where
			// they actually live -- a caller-owned 16-digit directory outside
			// the store would otherwise pass all of them. Session.Remove has
			// used this check since the symlink fix; the sweep must agree.
			if !sessionID(e.Name()) || !safeStore(cand) {
				live++ // not ours; its presence keeps the root registered
				continue
			}
			if _, err := os.Stat(filepath.Join(Root(), e.Name())); err == nil {
				live++
				continue
			}
			if os.RemoveAll(cand) == nil {
				removed++
			}
		}
		if live > 0 {
			keep = append(keep, root)
			continue
		}
		os.Remove(undoDir) // only succeeds when empty
	}
	if len(keep) != len(roots) {
		sort.Strings(keep)
		body := ""
		if len(keep) > 0 {
			body = strings.Join(keep, "\n") + "\n"
		}
		if err := os.WriteFile(rootsFile(), []byte(body), 0o600); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Unprotected counts the entries this session cannot restore.
//
// Three journal states land here, all of them already recorded by the shim: a
// `lost` record, meaning no backup could be taken at all; a backup that
// existed and was discarded when its store was destroyed; and a rename whose
// overwritten target could not be saved. They reduce to one rule -- a "-"
// backup whose method is anything other than "none".
//
// The method has to be consulted rather than just testing for "-", because
// `rename old new - none` is a rename that overwrote nothing, needed no
// backup, and restores perfectly. It is the most common entry in a real
// journal, and counting it would make the warning fire on almost every
// session -- a warning that cries wolf is one nobody reads.
func (s *Session) Unprotected() int {
	n := 0
	for _, e := range s.Entries {
		if e.Corrupt {
			n++
			continue
		}
		if e.Op == journal.OpLost {
			n++
			continue
		}
		if e.Backup() == "-" && e.Method() != "none" {
			n++
		}
	}
	return n
}

// inStore reports whether path sits directly inside a store directory
// belonging to session id -- that is, whether it looks like
// <root>/.undo/<id>/<name>.
//
// Backup paths come from the journal, which for this purpose is untrusted:
// a truncated write, a corrupted file, or a hand edit can name any path at
// all, and purge must not turn into an arbitrary rm. This is the check that
// makes deleting a journal-supplied path safe, so it is deliberately strict
// and structural rather than a prefix test.
func inStore(path, id string) bool {
	if id == "" || !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return false
	}
	dir := filepath.Dir(path)
	return filepath.Base(dir) == id && filepath.Base(filepath.Dir(dir)) == ".undo"
}

// removeDistributedBackups deletes the backups the shim placed outside the
// session directory, on the filesystem of the file each one protects, then
// removes the store directories they leave empty.
//
// Best-effort on purpose. A backup on a filesystem that is not mounted right
// now cannot be removed, and refusing to drop the session over it would leave
// a session nothing can ever delete. The gc orphan sweep is the backstop for
// exactly that case.
// realDir reports whether path is a directory in its own right rather than a
// symlink to one. Lstat does not follow the final component, so a symlink
// reports ModeSymlink and fails IsDir.
func realDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsDir()
}

// ownedByCaller reports whether path belongs to the user running this process.
func ownedByCaller(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// safeStore reports whether storeDir is a store we may delete inside: it and
// its parent .undo must both be real directories, and the store itself must
// belong to us.
//
// inStore checks the shape of a path; this checks what is actually on disk,
// and both are needed. The shape check is purely lexical, so if
// <root>/.undo/<id> is a symlink then os.Remove follows it and purge deletes
// files somewhere else entirely. On shared storage another user can arrange
// exactly that: the shim's mkdir gets EEXIST on the existing symlink and
// carries on.
//
// The .undo directory itself is not required to be ours. On a shared volume it
// may legitimately have been created by whoever ran undo there first, with our
// own <id> directory inside it.
func safeStore(storeDir string) bool {
	return realDir(storeDir) && ownedByCaller(storeDir) &&
		realDir(filepath.Dir(storeDir))
}

// It reports the paths it actually unlinked. Only a successful unlink proves
// the space came back: a path that has merely stopped resolving may be on a
// filesystem that went away mid-sweep, still holding the bytes.
func (s *Session) removeDistributedBackups() map[string]bool {
	prefix := s.Dir + string(os.PathSeparator)
	gone := make(map[string]bool)
	stores := make(map[string]bool)
	for _, p := range s.backupPaths() {
		if strings.HasPrefix(p, prefix) {
			continue // inside the session dir; RemoveAll gets it
		}
		if !inStore(p, s.ID) {
			continue // not ours to delete
		}
		if !safeStore(filepath.Dir(p)) {
			continue // right shape, wrong thing on disk
		}
		if os.Remove(p) == nil {
			gone[p] = true
		}
		stores[filepath.Dir(p)] = true
	}
	for d := range stores {
		// Both only succeed when empty, which is what keeps a store shared
		// with another session, and a .undo shared with another store, intact.
		os.Remove(d)
		os.Remove(filepath.Dir(d))
	}
	return gone
}

// Remove deletes a session and its backups entirely, including the ones held
// on other filesystems. The journal names those, so it has to go last.
func (s *Session) Remove() error {
	_, err := s.removeReporting()
	return err
}

// removeReporting is Remove, also reporting which distributed backups it
// unlinked so a caller tracking space can credit only those.
func (s *Session) removeReporting() (map[string]bool, error) {
	gone := s.removeDistributedBackups()
	return gone, os.RemoveAll(s.Dir)
}

// MarkUndone records that a session was reverted, and when.
func (s *Session) MarkUndone() error {
	at := time.Now()
	body := at.Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(filepath.Join(s.Dir, "undone"), []byte(body), 0o644); err != nil {
		return err
	}
	s.Undone = true
	s.UndoneAt = at
	return nil
}

// ClearUndone flips a session back to the applied state after a redo.
func (s *Session) ClearUndone() error {
	err := os.Remove(filepath.Join(s.Dir, "undone"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s.Undone = false
	s.UndoneAt = time.Time{}
	return nil
}
