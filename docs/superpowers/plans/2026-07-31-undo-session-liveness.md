# Cross-Node Session Liveness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `undo gc` from deleting the backups of a command that is still
running on another node, and stop a pruned session from evicting older sessions
for space it no longer occupies.

**Architecture:** A session records which kernel instance created it
(`hostname` + boot id) in a new `host` file. `Session.Live()` trusts the local
`kill(pid, 0)` probe only when that origin matches this host; a session from a
different origin is treated as live until it exceeds a configurable grace
period. The file is additive, so old binaries ignore it and sessions without
one keep today's behaviour exactly. Separately, GC's running byte total is
decremented when a session is actually removed.

**Tech Stack:** Go 1.24 (`internal/session`), POSIX shell hooks (bash/zsh/fish),
bash e2e harness.

## Global Constraints

- The shim must never make the user's command fail. Assert it by test.
- glibc floor must stay <= `GLIBC_2.34`. Check with `objdump -T` after any shim
  change. **This plan touches no shim source**, so the floor cannot move; verify
  anyway at the end.
- No site data in the public repo. Run `tools/check-no-site-data.sh` and
  `tools/check-ere.sh` before every commit. Nothing is exempt.
- Upstream CONTRIBUTING: any shim change needs an e2e case. No shim change here,
  but Task 3 adds an e2e case regardless because the defect is only observable
  end to end.
- Build and test only via `test/in-container.sh <cmd>`.
- macOS ships bash 3.2: no `mapfile`, no bare `"${arr[@]}"` under `set -u`.
- The session store is shared across nodes via a shared `$HOME`. Anything
  process-local (a pid) is meaningless to a reader on another node.

## Background: the defect, as reproduced

A session records `pid` and nothing about which machine wrote it. `Live()` is a
local `kill(pid, 0)`. A command still running on another node has no `done`
marker and a pid that means nothing locally, so it reads as finished and enters
ordinary retention at `internal/session/session.go:347`.

Reproduced 2026-07-31 in the container with stock defaults (`UNDO_KEEP=30`, no
budget pressure), 31 subsequent commands in another shell:

```
REPRODUCED: the running session was deleted while its command was still running
REPRODUCED: its backup was deleted -- the file it protects is now unrecoverable
REPRODUCED: session directory deleted mid-command
REPRODUCED: backup of early.txt deleted mid-command
not reproduced: the command still succeeded (shim did not break it)
REPRODUCED: no journal remains: every record written after gc went to an unlinked inode
REPRODUCED: the long job is not listed at all -- the user has nothing to undo
```

The shim caches its journal descriptor per thread (`shim/undo_shim.c:51-70`) and
never reopens it while `UNDO_SESSION` is unchanged, so after the directory is
deleted it appends to an unlinked inode. Files deleted *before* GC ran lose
their backups; files deleted *after* are never recorded. Both are unrecoverable
and nothing is printed.

## Residual risk this plan does NOT close

**A reused pid pins a session forever.** If a command dies without writing
`done` — its shell killed — and the pid is later reissued to something else,
`probe()` reports the session live indefinitely. It is then never collected,
and `undo apply` and `purge` refuse it. Raised by the gate; deliberately not
fixed here, for two reasons. It is pre-existing and unchanged by this plan, and
it has an escape: `--force` overrides both refusals, though only `cmdApply`
says so — `cmdPurge` prints the skip and the pid without mentioning the flag
(`cmd/undo/main.go:88`, `:242`). More importantly the obvious fix — bounding the
local probe by the grace as well — would trade a leak with an escape hatch for
silent data loss on any local command that runs longer than the grace, which is
the wrong direction. Fixing it properly means detecting reuse rather than
timing it out: compare the pid's start time (`/proc/<pid>/stat` field 22,
against boot time) with the session's own, since a process that started after
the session cannot be its creator. That needs no new recorded field, but it
does need boot-time arithmetic that behaves under container and NTP-step
conditions, so it belongs in its own change.

The grace period is bounded by `Started()`, which is when the command began. A
foreign command that runs longer than the grace becomes collectible again while
still running. Closing that needs a liveness signal the running process
refreshes (a heartbeat file touched by the shim), which is per-session periodic
metadata write traffic on a shared home — exactly the load question still open
with the storage admins. Do not add a heartbeat in this plan. State the bound
in the README so the default is a decision, not an accident.

## File Structure

| File | Responsibility |
|---|---|
| `internal/session/session.go` | `thisHost()`, `Origin` field, `load()` reads it, `Create()` writes it, `Live()` uses it, GC total decrement |
| `shell/undo.bash`, `shell/undo.zsh`, `shell/undo.fish` | compute the origin once at source time, write `$dir/host` per session |
| `internal/session/session_test.go` | liveness matrix, GC regression, watermark |
| `test/e2e.sh` | `run_armed` writes `host`; end-to-end regression cases |
| `README.md` | document `UNDO_FOREIGN_GRACE` and the bound |

---

### Task 1: Record which kernel instance created a session

Pure recording. No behaviour changes; `Live()` is untouched. A reviewer can
accept this and still reject the policy in Task 2.

**Files:**
- Modify: `internal/session/session.go` (package doc, `Session` struct, `load`, `Create`)
- Modify: `shell/undo.bash`, `shell/undo.zsh`, `shell/undo.fish`
- Modify: `test/e2e.sh` (`run_armed`)
- Test: `internal/session/session_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func thisHost() string`; `Session.Origin string` (empty when the
  session has no `host` file). Task 2 consumes both.

- [ ] **Step 1: Write the failing test**

Add to `internal/session/session_test.go`:

```go
func TestCreateRecordsOrigin(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, "host"))
	if err != nil {
		t.Fatalf("no host file: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != thisHost() {
		t.Errorf("host file = %q, want %q", got, thisHost())
	}
	got, err := Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != thisHost() {
		t.Errorf("loaded Origin = %q, want %q", got.Origin, thisHost())
	}
}

// A session written by an older hook has no host file. It must load cleanly
// with an empty Origin, which Live treats as "use the pid probe, as before".
func TestLoadWithoutOriginIsEmpty(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s, err := Create("rm x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Dir, "host")); err != nil {
		t.Fatal(err)
	}
	got, err := Get(s.ID)
	if err != nil {
		t.Fatalf("a session without a host file must still load: %v", err)
	}
	if got.Origin != "" {
		t.Errorf("Origin = %q, want empty", got.Origin)
	}
}

// thisHost must be stable within a process: Live compares against it on every
// call, and a value that varied would make liveness flap.
//
// Deliberately does not assert non-empty. The contract permits an empty result
// -- a host where neither the hostname nor the boot id can be read -- and Live
// resolves that case explicitly. Asserting otherwise would make this test fail
// on a restricted container even where the fallback is working correctly.
func TestThisHostIsStableAndOneLine(t *testing.T) {
	a, b := thisHost(), thisHost()
	if a != b {
		t.Errorf("thisHost not stable: %q then %q", a, b)
	}
	if strings.Contains(a, "\n") {
		t.Errorf("thisHost must be one line, got %q", a)
	}
}

// Half an identity is worse than none: it matches sessions it should not.
func TestComposeHostRequiresBothHalves(t *testing.T) {
	if got := composeHost("node1", ""); got != "" {
		t.Errorf("hostname with no boot id = %q, want empty: a session from "+
			"before a reboot would look local and its pid may be reissued", got)
	}
	if got := composeHost("", "boot-uuid"); got != "" {
		t.Errorf("boot id with no hostname = %q, want empty: containers sharing "+
			"a kernel have separate pid namespaces", got)
	}
	if got := composeHost("node1", "boot-uuid"); got != "node1\tboot-uuid" {
		t.Errorf("composeHost = %q", got)
	}
}

// The shell hooks and Create must agree on what identifies this host, or a
// session created by the fish hook reads as foreign to the Go binary running
// on the same machine -- pinned for the whole grace, and undo refusing to
// revert it. `uname -n` is gethostname(2) by POSIX definition, which is what
// bash's $HOSTNAME, zsh's $HOST and Go's os.Hostname all report. `hostname`
// the command is not: it may print the FQDN where gethostname returns the
// short name.
func TestUnameMatchesGoHostname(t *testing.T) {
	out, err := exec.Command("uname", "-n").Output()
	if err != nil {
		t.Skipf("uname unavailable: %v", err)
	}
	name, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != name {
		t.Errorf("uname -n = %q but os.Hostname = %q; the hooks and the binary "+
			"would disagree about which sessions are local", got, name)
	}
}
```

Add `"os/exec"` to the test file's imports.

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/session/ -run 'Origin|ThisHost|Uname|ComposeHost' -v`
Expected: FAIL — `undefined: thisHost`, and `Session` has no field `Origin`.
`TestUnameMatchesGoHostname` should PASS from the start: it pins an agreement
between the hooks and the binary that must not drift, not a change being made.

- [ ] **Step 3: Implement the recording**

In `internal/session/session.go`, add `"sync"` to the imports.

Extend the package doc block (after the `data/` line):

```go
//	pid      - the pid of the shell or runner that started the command
//	host     - the kernel instance that created the session: hostname, a tab,
//	           and the boot id. Homes are shared across nodes, so a pid alone
//	           does not say whether the command is still running; see Live.
```

Add to the `Session` struct, after `Pid`:

```go
	Origin   string // host+boot id that created it, empty for older sessions
```

Add near `Root()`:

```go
var (
	hostOnce sync.Once
	hostID   string
)

// thisHost identifies the running kernel instance: hostname and boot id.
//
// A pid is only meaningful within one of these. The boot id alone would not
// do -- containers on one host share it while having separate pid namespaces
// -- and the hostname alone would not survive a reboot, so both are recorded
// and compared together. If neither can be read the result is empty, which
// Live treats as "cannot compare" and resolves in the safe direction.
func thisHost() string {
	hostOnce.Do(func() {
		name, _ := os.Hostname()
		var boot string
		if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
			boot = strings.TrimSpace(string(b))
		}
		hostID = composeHost(name, boot)
	})
	return hostID
}

// composeHost joins the two halves of a host identity, or returns empty if
// either is missing.
//
// Both or neither. A hostname without a boot id would make a session from
// before a reboot look local, and Live would then trust a pid that has since
// been reissued to something else. A boot id without a hostname would make
// containers sharing a kernel look like one another while their pid namespaces
// are separate. Half of this is not a weaker version of it; it is a different
// and wrong answer.
func composeHost(name, boot string) string {
	if name == "" || boot == "" {
		return ""
	}
	return name + "\t" + boot
}
```

In `load()`, after the `pid` block:

```go
	if b, err := os.ReadFile(filepath.Join(dir, "host")); err == nil {
		s.Origin = strings.TrimSpace(string(b))
	}
```

In `Create()`, after the `pid` write and before the `return`:

```go
	// Written as its own file rather than folded into pid: during a rollout
	// the binary and the shell hooks update at different times, and an older
	// undo reading a newer session must not trip over a changed pid format.
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(thisHost()+"\n"), 0o600); err != nil {
		return nil, err
	}
```

and set the field on the returned value:

```go
	return &Session{ID: id, Dir: dir, Cmd: cmd, Pid: os.Getpid(), Origin: thisHost()}, nil
```

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `test/in-container.sh go test ./internal/session/ -run 'Origin|ThisHost|Uname|ComposeHost' -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Write the same origin from the three shell hooks**

The origin is computed once when the hook is sourced, not per command: neither
component can change without ending the shell, and `_undo_preexec` runs before
every command the user types.

In `shell/undo.bash`, after the `chmod 700` line near the top:

```bash
# identifies this kernel instance; see Session.Live. Computed once: a reboot
# ends this shell, and the hostname does not change under it.
_undo_boot=
[[ -r /proc/sys/kernel/random/boot_id ]] && read -r _undo_boot < /proc/sys/kernel/random/boot_id
_undo_origin=${HOSTNAME:-}$'\t'$_undo_boot
```

and in `_undo_preexec`, immediately after the `pid` write:

```bash
    printf '%s\n' "$_undo_origin" >| "$dir/host"
```

In `shell/undo.zsh`, after its `chmod 700` line:

```zsh
_undo_boot=
[[ -r /proc/sys/kernel/random/boot_id ]] && read -r _undo_boot < /proc/sys/kernel/random/boot_id
typeset -g _undo_origin=${HOST:-}$'\t'$_undo_boot
```

and after `print -r -- $$ >| $dir/pid`:

```zsh
    print -r -- $_undo_origin >| $dir/host
```

In `shell/undo.fish`, after its `chmod 700` line:

```fish
set -l _undo_boot ""
if test -r /proc/sys/kernel/random/boot_id
    read -l _undo_boot </proc/sys/kernel/random/boot_id
end
# uname -n, not hostname: uname -n is gethostname(2), which is what bash's
# $HOSTNAME, zsh's $HOST and Go's os.Hostname all report. `hostname` may print
# the FQDN instead, and a session that disagrees with the binary about its own
# host reads as foreign on the machine that created it.
set -g _undo_origin (string join \t (uname -n) $_undo_boot)
```

and after `printf '%s\n' $fish_pid >$dir/pid`:

```fish
    printf '%s\n' $_undo_origin >$dir/host
```

- [ ] **Step 6: Make the e2e harness record it too**

`run_armed` stands in for a shell hook, so it must write what a hook writes,
or no e2e case ever exercises the same-host path with an origin present.

In `test/e2e.sh`, in `run_armed`, after the `cmd` write:

```bash
    printf '%s\t%s\n' "$(uname -n)" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)" >"$sess/host"
```

- [ ] **Step 7: Verify nothing regressed**

Run: `test/in-container.sh make test`
Expected: PASS, all existing e2e cases unchanged. Case 14 ("refuses to undo a
session whose command may be running") now runs the same-host path instead of
the legacy path and must still pass, because both probe the pid.

Run: `test/in-container.sh bash test/hook-zsh.sh`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/session.go internal/session/session_test.go shell/ test/e2e.sh
git diff --cached --stat
git commit -m "session: record which kernel instance created a session"
```

---

### Task 2: Trust the pid probe only within its own kernel instance

**Files:**
- Modify: `internal/session/session.go` (`Live`, new `probe`, `withinGrace`,
  `foreignGrace`, `envSeconds`, `minForeignGrace`, and `GC`'s empty-session arm)
- Modify: `README.md`
- Test: `internal/session/session_test.go`

**Interfaces:**
- Consumes: `thisHost()`, `Session.Origin` from Task 1.
- Produces: `func foreignGrace() time.Duration`; changed `Live()` semantics.
  Task 3 asserts them end to end.

- [ ] **Step 1: Write the failing test**

Add to `internal/session/session_test.go`:

```go
// foreignSession builds a session that looks like one written by another node:
// an origin that is not ours, a pid that does not resolve here, no done marker.
func foreignSession(t *testing.T, age time.Duration) *Session {
	t.Helper()
	start := time.Now().Add(-age)
	id := fmt.Sprintf("%d%06d", start.Unix(), start.Nanosecond()/1000)
	dir := filepath.Join(Root(), id)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	// a pid that is not running here; 0x7FFFFFFF is above every pid_max
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte("othernode\tnot-our-boot-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLiveTreatsAForeignRunningSessionAsLive(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Hour)
	if !s.Live() {
		t.Error("a session from another node with no done marker was read as finished; " +
			"gc would delete the backups of a command that is still running")
	}
}

// A foreign session whose pid file has not been written yet -- or was written
// short, or cannot be parsed -- loads with Pid 0. That must not be read as
// "finished": on a shared store it is the ordinary look of a session whose
// creator is still setting it up.
func TestLiveDoesNotConsultThePidOfAForeignSession(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.Remove(filepath.Join(s.Dir, "pid")); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pid != 0 {
		t.Fatalf("setup: want Pid 0, got %d", s.Pid)
	}
	if !s.Live() {
		t.Error("a foreign session with no readable pid was called finished")
	}
}

// The grace is what keeps an abandoned foreign session from being pinned
// forever: past it, the session is collectible again.
func TestLiveExpiresAForeignSessionPastTheGrace(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	t.Setenv("UNDO_FOREIGN_GRACE", "3600")
	s := foreignSession(t, 2*time.Hour)
	if s.Live() {
		t.Error("a foreign session older than the grace must be collectible")
	}
}

// withLocalHost forces thisHost's answer for one test. hostOnce is marked done
// so the real lookup cannot overwrite it afterwards.
func withLocalHost(t *testing.T, id string) {
	t.Helper()
	hostOnce.Do(func() {})
	prev := hostID
	hostID = id
	t.Cleanup(func() { hostID = prev })
}

// With no local identity nothing can be classified, so neither signal is sound
// alone: a session is finished only when the probe and the grace agree.
// Otherwise a local command that outlived the grace could be purged, or an
// undo applied over it, while it was still writing.
func TestLiveWithNoLocalIdentityNeedsBothSignals(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	withLocalHost(t, "")

	// far past any grace, but its pid is ours and demonstrably running
	s := foreignSession(t, 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("a running pid must still count as live when the local identity " +
			"is unknown; purge and apply would act on a running command")
	}
}

// The grace cannot be tuned below the floor: a grace shorter than the clock
// skew between two nodes would collect a command that just started.
func TestForeignGraceHasAFloor(t *testing.T) {
	t.Setenv("UNDO_FOREIGN_GRACE", "1")
	if got := foreignGrace(); got != minForeignGrace {
		t.Errorf("grace = %v, want the floor %v", got, minForeignGrace)
	}
}

// A foreign session that finished normally left a done marker, and that is
// conclusive from any node -- no grace needed.
func TestLiveHonoursDoneOnAForeignSession(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.WriteFile(filepath.Join(s.Dir, "done"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("a foreign session with a done marker is finished")
	}
}

// Sessions written before this change have no origin. They must keep the old
// behaviour exactly, or a rollout strands every session already on disk.
func TestLiveFallsBackToTheProbeWithoutAnOrigin(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.Remove(filepath.Join(s.Dir, "host")); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Live() {
		t.Error("without an origin the pid probe decides, and this pid is not running")
	}
}

// Same host: the probe is meaningful and must still be what decides.
func TestLiveProbesThePidOnItsOwnHost(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	s := foreignSession(t, time.Minute)
	if err := os.WriteFile(filepath.Join(s.Dir, "host"), []byte(thisHost()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := load(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Live() {
		t.Error("our own running pid on our own host must read as live")
	}
}
```

Add `"fmt"` and `"strconv"` to the test file's imports.

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/session/ -run 'TestLive|TestForeignGrace' -v`
Expected: the package does not build — `undefined: foreignGrace`,
`undefined: minForeignGrace`, `undefined: withinGrace`. That is the honest
failure here, not a red test: the tests name symbols Step 3 introduces.

So this step cannot show which tests discriminate; Step 5 does that instead, by
reverting the logic while keeping the symbols. Of the eight, two carry the
change — `TestLiveTreatsAForeignRunningSessionAsLive` and
`TestLiveDoesNotConsultThePidOfAForeignSession`. The other six pass both before
and after by design: they pin behaviour that must not move.

- [ ] **Step 3: Implement**

In `internal/session/session.go`, replace `Live()` with:

```go
// Live reports whether the session's command may still be running.
//
// A pid only means something on the kernel instance that issued it, and homes
// are shared across nodes, so the local probe is trusted only when the origin
// matches. A session from elsewhere cannot be probed at all: it is treated as
// running until it outlives foreignGrace, because deleting a running command's
// backups destroys data while keeping a finished session merely wastes space.
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

// foreignGrace is how long a session from another node is presumed to be
// running when it has no done marker.
//
// Normal termination writes that marker, so this only has to cover abnormal
// ends -- a killed job, a crashed node -- which makes a generous default
// cheap. The bound is real: a command running longer than this becomes
// collectible while still running. Raise it where jobs outlive it.
func foreignGrace() time.Duration {
	g := time.Duration(envSeconds("UNDO_FOREIGN_GRACE", 7*24*60*60)) * time.Second
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
```

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `test/in-container.sh go test ./internal/session/ -run 'TestLive|TestForeignGrace' -v`
Expected: PASS (8 tests).

Run: `test/in-container.sh go test ./...`
Expected: PASS.

- [ ] **Step 5: Prove the new tests discriminate**

Temporarily revert the foreign-origin branch in `Live()` (the `local :=` line
through the closing brace) and rerun. Confirm that both
`TestLiveTreatsAForeignRunningSessionAsLive` and
`TestLiveDoesNotConsultThePidOfAForeignSession` FAIL. Restore.

Then separately restore that branch but move the `Pid <= 0` check back above it,
and confirm `TestLiveDoesNotConsultThePidOfAForeignSession` FAILS on its own —
that ordering is the whole content of that test, and it was the first thing the
gate caught. Restore.

A regression test that passes with and without the fix is not evidence.

- [ ] **Step 6: Close the creation race on empty sessions**

Found by the gate over this plan. `GC` deletes any non-live session with no
journal entries (`internal/session/session.go:350-355`). A session is a
directory before it is anything else: the hook runs `mkdir -p`, then writes
`cmd`, `pid` and `host`, and the journal appears only when the command first
touches a file. In the window before `pid` exists, `load` yields `Pid` 0,
`Live()` is false, and the session is deleted out from under the command about
to use it. Sub-millisecond on one node; on a shared home an attribute cache
makes it longer.

The `done` marker is what distinguishes the two kinds of empty session, so the
fence costs nothing anywhere else.

Write the failing test:

```go
// A session that has been created but has not yet recorded anything, and has
// not been marked done, may be moments from its first entry -- or may be one
// whose creator is on another node and still writing it out. Deleting it takes
// the directory the command is about to journal into.
func TestGCSparesAYoungSessionThatHasRecordedNothingYet(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	starting, err := Create("a command that has not touched a file yet")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(starting.Dir, "pid")); err != nil {
		t.Fatal(err) // as a reader sees it before the pid is written
	}
	if _, err := GC(30, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(starting.Dir); err != nil {
		t.Error("gc deleted a session that was still being set up")
	}
}
```

Run: `test/in-container.sh go test ./internal/session/ -run TestGCSparesAYoung -v`
Expected: FAIL — the session directory is gone.

Implement, in `GC`'s empty-session arm:

```go
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
```

An unfinished empty session therefore survives up to a week. That is four small
files: the hook writes `done` at the next prompt, so the ones that linger are
only those killed before it ran.

Run: `test/in-container.sh go test ./internal/session/ -v`
Expected: PASS. `TestGCRemovesEmptyAndOversized`
(`internal/session/session_test.go:40`) creates an empty session and expects it
collected; it calls `MarkDone` first, so the `!s.Done` guard leaves it
collectible. e2e case 15 uses id `1111111111111111`, dated 2005 and far past the
grace. Neither should need changing — if either does, stop and re-read it before
touching it.

- [ ] **Step 7: Close the same hole in the hook fallbacks**

Found by the gate over this plan. Each hook has a fallback pruner used when the
`undo` binary is not on `PATH`, and all three delete sessions directly:
`shell/undo.bash:80-92`, `shell/undo.zsh:86-94`, `shell/undo.fish:95-107`. They
check neither `done` nor liveness, and they prune by count with `rm -rf`, so on
a shared home they delete another node's *running* session outright — the defect
this plan exists to fix, untouched on this path. They also remove the session
directory without its distributed backups, which orphans those beyond the sweep,
since only `undo gc` registers store roots.

A `done` marker is conclusive from any node, so requiring one is the whole fix.
The count-based prune goes entirely: it cannot be made safe here, and letting
the store grow until `undo` is available is the recoverable direction.

In `shell/undo.bash`, replace the fallback block:

```bash
    # fallback: only sessions this shell can prove are finished and empty.
    # Without a done marker a session may belong to a command still running --
    # on this machine or another one, since the store is shared. Pruning by
    # count is gone: it had no liveness check at all, and it removed session
    # directories without their distributed backups, stranding those where
    # only `undo gc` can still find them.
    local d
    for d in "$UNDO_DATA_DIR"/sessions/*/; do
        [[ -d $d ]] || continue
        [[ -f $d/done ]] || continue
        [[ -s $d/journal ]] || rm -rf -- "$d"
    done
```

In `shell/undo.zsh`, replace the `else` arm:

```zsh
    else
        # only finished, empty sessions -- see the bash hook for why
        local d
        for d in $UNDO_DATA_DIR/sessions/*(N/); do
            [[ -f $d/done ]] || continue
            [[ -s $d/journal ]] || command rm -rf -- $d
        done
    fi
```

In `shell/undo.fish`, replace the fallback block:

```fish
    # only finished, empty sessions -- see the bash hook for why
    for d in $UNDO_DATA_DIR/sessions/*/
        if test -f $d/done; and not test -s $d/journal
            command rm -rf -- $d
        end
    end
```

`UNDO_KEEP` was read only by the pruners now removed, so its default assignment
in each hook is dead. Delete it: `shell/undo.bash:7`, `shell/undo.zsh:7`,
`shell/undo.fish:6`. The Go binary reads `UNDO_KEEP` from the environment, and
these assignments were unexported shell variables it never saw.

Run: `test/in-container.sh bash test/hook-zsh.sh`
Expected: PASS. It does not exercise the count pruning or those assignments.

- [ ] **Step 8: Document the variable and its bound**

In `README.md`, alongside `UNDO_KEEP` and `UNDO_MAX_STORE`:

```markdown
- `UNDO_FOREIGN_GRACE` — seconds a session created on another machine is
  presumed to be still running when it has no completion marker (default
  604800, one week; values below 15 minutes are raised to it). Only matters
  when the session store is on a shared home directory. A command that runs
  longer than this can have its backups collected while it is still running;
  raise it above your longest job.
```

- [ ] **Step 9: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/session.go internal/session/session_test.go README.md shell/
git diff --cached --stat
git commit -m "session: a pid means nothing on a node that did not issue it"
```

---

### Task 3: Prove it end to end

The unit tests pin `Live()`. This task pins the thing that actually went wrong:
GC deleting a running command's backups. It is separable because a reviewer
could accept the policy in Task 2 and still find this proof inadequate.

**Files:**
- Test: `internal/session/session_test.go`
- Test: `test/e2e.sh`

**Interfaces:**
- Consumes: `Live()` from Task 2, `foreignSession` helper from Task 2's tests.
- Produces: nothing consumed later.

- [ ] **Step 1: Write the failing GC-level test**

```go
// The reproduced defect: ordinary retention, no budget pressure, and a command
// running on another node loses its session and its backups mid-flight.
func TestGCKeepsASessionRunningOnAnotherNode(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	running := foreignSession(t, time.Hour)
	bak := filepath.Join(vol, ".undo", running.ID, "1360-1")
	writeBackup(t, bak, 64)
	if err := os.WriteFile(filepath.Join(running.Dir, "journal"),
		[]byte("unlink\t"+filepath.Join(vol, "gone.txt")+"\t"+bak+"\tlink\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// the user's login shell, meanwhile: more commands than UNDO_KEEP
	for i := 0; i < 5; i++ {
		s, err := Create(fmt.Sprintf("rm filler-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, "journal"),
			[]byte("unlink\t/x\t-\tnone\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDone(); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := GC(2, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(running.Dir); err != nil {
		t.Error("gc deleted the session of a command still running on another node")
	}
	if _, err := os.Stat(bak); err != nil {
		t.Error("gc deleted the backup of a command still running on another node")
	}
}
```

- [ ] **Step 2: Run it**

Run: `test/in-container.sh go test ./internal/session/ -run TestGCKeeps -v`
Expected: PASS with Task 2 applied. Then temporarily revert the `s.Origin`
branch in `Live()` and confirm it FAILS with both errors. Restore.

- [ ] **Step 3: Add the e2e case**

Append to `test/e2e.sh`, renumbering to follow the last existing case:

```bash
echo "== case 33: a command running on another node keeps its backups"
make_tree
run_armed "rm $PLAY/top.txt"
last=$(ls "$UNDO_DATA_DIR/sessions" | sort | tail -1)
sess=$UNDO_DATA_DIR/sessions/$last
bak=$(awk -F'\t' '$1=="unlink"{print $3}' "$sess/journal" | head -1)
[[ -e $bak ]] || fail "case 33 setup: no backup was taken"
# what this session looks like from a different node of the cluster
printf 'othernode\tnot-our-boot-id\n' >"$sess/host"
echo 2147483647 >"$sess/pid"
rm -f "$sess/done"
for i in 1 2 3 4 5; do
    echo "filler $i" >"$PLAY/filler-$i.txt"
    run_armed "rm $PLAY/filler-$i.txt"
done
UNDO_KEEP=2 "$UNDO" gc >/dev/null
[[ -d $sess ]] || fail "gc deleted a session still running on another node"
[[ -e $bak ]] || fail "gc deleted the backup of a command still running elsewhere"

echo "== case 34: a foreign session past its grace is collectible again"
# Built with an explicitly old id rather than by aging a fresh session: the
# grace has a 15-minute floor, so "wait for it to expire" is not a test.
old=$(( $(date +%s) - 3600 ))000000
oldsess=$UNDO_DATA_DIR/sessions/$old
mkdir -p "$oldsess/data"
echo "a command that died on another node" >"$oldsess/cmd"
printf 'othernode\tnot-our-boot-id\n' >"$oldsess/host"
echo 2147483647 >"$oldsess/pid"
oldstore=$PLAY/.undo/$old
mkdir -p "$oldstore"
echo "backed up" >"$oldstore/1-1"
printf 'unlink\t%s\t%s\tlink\n' "$PLAY/oldfile.txt" "$oldstore/1-1" >"$oldsess/journal"
UNDO_KEEP=2 UNDO_FOREIGN_GRACE=1800 "$UNDO" gc >/dev/null
[[ ! -d $oldsess ]] || fail "a foreign session past its grace must be collectible"
[[ ! -e $oldstore/1-1 ]] || fail "its backup should have been reclaimed too"
```

- [ ] **Step 4: Run the full suite**

Run: `test/in-container.sh make test`
Expected: PASS, including the new case 33.

- [ ] **Step 5: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/session_test.go test/e2e.sh
git diff --cached --stat
git commit -m "test: a command running on another node keeps its backups"
```

---

### Task 4: Stop charging GC for space it has already reclaimed

Independent of Tasks 1-3 and safe to drop if review stalls.

**Files:**
- Modify: `internal/session/session.go` (`allocatedBytes` split, new
  `backupCharge`, `chargeTotal`, `distributedCharges`, `GC`)
- Test: `internal/session/session_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: nothing consumed later.

- [ ] **Step 1: Write the failing test**

```go
// mustSession creates a done session. Create's own collision loop guarantees
// ids are distinct and ordered, so no sleep is needed between calls.
func mustSession(t *testing.T, cmd string) *Session {
	t.Helper()
	s, err := Create(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(); err != nil {
		t.Fatal(err)
	}
	return s
}

// GC walks newest first and keeps a running total. When it prunes a session
// that total must come down, or every older session is evicted for space that
// is no longer occupied -- including sessions that allocate nothing at all.
func TestGCDoesNotEvictForSpaceItAlreadyFreed(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()

	// Backups must sit at <root>/.undo/<session-id>/<name> or Remove will not
	// touch them (see inStore), and the test would pass for the wrong reason.

	// oldest: costs nothing, its backup is a hardlink
	cheap := mustSession(t, "rm cheap")
	cheapBak := filepath.Join(vol, ".undo", cheap.ID, "1-1")
	writeBackup(t, cheapBak, 8)
	writeJournal(t, cheap, "unlink\t/a\t"+cheapBak+"\tlink\n")

	// newer: one big copy, alone over the budget
	big := mustSession(t, "sort -o big big")
	bigBak := filepath.Join(vol, ".undo", big.ID, "1-1")
	writeBackup(t, bigBak, 8192)
	writeJournal(t, big, "mod\t/b\t"+bigBak+"\tcopy\n")

	// newest: exempt from eviction, so it is not what evicts the others
	newest := mustSession(t, "rm newest")
	writeJournal(t, newest, "unlink\t/c\t-\tnone\n")

	if _, err := GC(30, 3000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bigBak); err == nil {
		t.Error("the over-budget session should have been pruned")
	}
	if _, err := os.Stat(cheap.Dir); err != nil {
		t.Error("a session that allocates nothing was evicted for space gc had already freed")
	}
}

// A backup Remove could not delete still occupies space. Crediting it back
// would let the store sit over budget while gc believed it had made room.
//
// The undeletable backup is arranged with a symlinked store directory, which
// removeDistributedBackups refuses to delete through (safeStore). That works
// regardless of the uid running the test, unlike a read-only parent, which
// root ignores.
func TestGCDoesNotCreditBackupsItFailedToDelete(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	vol := t.TempDir()
	elsewhere := t.TempDir()

	cheap := mustSession(t, "rm cheap")
	writeJournal(t, cheap, "unlink\t/a\t-\tnone\n")

	big := mustSession(t, "sort -o big big")
	realStore := filepath.Join(elsewhere, "store")
	if err := os.MkdirAll(realStore, 0o700); err != nil {
		t.Fatal(err)
	}
	survivor := filepath.Join(realStore, "1-1")
	writeBackup(t, survivor, 8192)
	if err := os.MkdirAll(filepath.Join(vol, ".undo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realStore, filepath.Join(vol, ".undo", big.ID)); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, big, "mod\t/b\t"+filepath.Join(vol, ".undo", big.ID, "1-1")+"\tcopy\n")

	newest := mustSession(t, "rm newest")
	writeJournal(t, newest, "unlink\t/c\t-\tnone\n")

	if _, err := GC(30, 3000, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(survivor); err != nil {
		t.Fatalf("setup: the backup was meant to survive Remove: %v", err)
	}
	if _, err := os.Stat(cheap.Dir); err == nil {
		t.Error("gc credited back bytes it never freed and stopped pruning; " +
			"the store is now over budget and believes it is not")
	}
}

// The count limit and the byte limit are separate rules; this one covers the
// count so a change to either is not masked by the other.
func TestGCKeepsNoMoreThanKeepSessions(t *testing.T) {
	t.Setenv("UNDO_DATA_DIR", t.TempDir())
	for i := 0; i < 5; i++ {
		s := mustSession(t, fmt.Sprintf("rm f%d", i))
		writeJournal(t, s, "unlink\t/f\t-\tnone\n")
	}
	if _, err := GC(2, 1<<30, 0); err != nil {
		t.Fatal(err)
	}
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > 2 {
		t.Errorf("gc left %d sessions, keep was 2", len(all))
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `test/in-container.sh go test ./internal/session/ -run TestGCDoesNotEvict -v`
Expected: FAIL — "a session that allocates nothing was evicted for space gc had
already freed".

- [ ] **Step 3: Implement**

First split `allocatedBytes` so the distributed half can be measured again after
a removal. Replace its body with:

```go
// backupCharge is one backup held outside the session directory, and what it
// costs. The size is captured up front because it cannot be recovered later:
// a backup on a volume that has gone away still occupies space, and stat will
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

func (s *Session) allocatedBytes() int64 {
	return dirSize(s.Dir) + chargeTotal(s.distributedCharges())
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
```

Add `"errors"` to the package imports; `io/fs` is already there.

Keep the existing doc comment on `allocatedBytes`; it explains the hardlink
exclusion and still applies.

Then in `GC`, rename `kept` to `seen` and hoist the size:

```go
	removed, seen := 0, 0
```

```go
		seen++
		dirBytes := dirSize(s.Dir)
		charges := s.distributedCharges()
		total += dirBytes + chargeTotal(charges)
```

`seen` counts sessions considered, not sessions retained, and is deliberately
not decremented on removal: `keep` means "the newest N", so everything past
position N goes regardless of what was freed ahead of it. The rename is so the
name stops implying otherwise. Update the two other uses (`seen == 1` and
`seen > keep`) and the eviction arm:

```go
		if seen > keep || total > maxBytes || tooOld {
			if s.Remove() == nil {
				removed++
				// Give back only what is verifiably gone. RemoveAll took the
				// session directory. For the backups outside it, only ENOENT
				// proves deletion: removeDistributedBackups is best effort,
				// and a volume that has gone away answers something else
				// entirely. Crediting those bytes back would let the store sit
				// over budget while gc believed it had made room -- so a
				// backup we cannot prove is gone keeps being charged.
				freed := dirBytes
				for _, c := range charges {
					if _, err := os.Lstat(c.path); errors.Is(err, fs.ErrNotExist) {
						freed += c.size
					}
				}
				total -= freed
			}
		}
```

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `test/in-container.sh go test ./internal/session/ -v`
Expected: PASS.

Run: `test/in-container.sh make test`
Expected: PASS. Case 32 asserts accounting by save method and was written
around this behaviour; if it now fails, re-read it before changing it — the
ordering it uses may no longer be necessary, but the accounting it checks is.

- [ ] **Step 5: Commit**

```bash
tools/check-no-site-data.sh && tools/check-ere.sh
git add internal/session/session.go internal/session/session_test.go
git diff --cached --stat
git commit -m "session: stop charging gc for space it already reclaimed"
```

---

## Final verification

- [ ] `test/in-container.sh make test` — all Go tests and every e2e case
- [ ] `test/in-container.sh --privileged test/multifs.sh` — the two-filesystem harnesses
- [ ] `test/in-container.sh bash test/hook-zsh.sh` — the zsh hook still loads
- [ ] `test/in-container.sh objdump -T build/libundo.so | grep GLIBC_ | sed 's/.*GLIBC_/GLIBC_/' | sort -u` — floor still `GLIBC_2.34`, unchanged since no shim source was touched
- [ ] `tools/check-no-site-data.sh && tools/check-ere.sh`
- [ ] Re-run the original reproduction (`repro-liveness.sh`) and confirm experiments A and B no longer reproduce
- [ ] `git fetch upstream && git log --oneline HEAD..upstream/main` — still current
