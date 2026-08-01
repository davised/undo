# Capturing agent-run commands: design

**Status:** proposed
**Date:** 2026-07-31
**Upstream:** `edaywalid/undo` (MIT)
**Companion to:** `undo-multifs-design.md`, whose phasing item 4 this is.

Site-agnostic by construction, like its companion. It describes classes of
host and filesystem, never named hosts, mounts, or organizations.

## Summary

`undo` protects interactive commands. An AI coding agent gets nothing: the
shell hooks need an interactive shell, and an agent's tool call is
`<shell> -c '...'`, which runs no `preexec` and no `PROMPT_COMMAND`. So the one
class of user most likely to run a destructive command it did not intend — and
to realise two messages later — is the one class with no recovery path.

Everything downstream of capture already works. `undo list`, `undo diff <id>`
and `undo apply <id>` revert one specific older session; restore refuses
per-entry when a path exists again (`internal/restore/restore.go:296`, `:320`,
`:366`, with `--force` to override); sessions with no journal entries cost no
retention slot. **The only missing piece is capture.**

This design adds it by arming through the inherited environment and having the
shim create sessions lazily, grouped by process group. It also closes a
pre-existing silent-data-loss path that the new arming would otherwise make
routine, and a journal-corruption path that quota exhaustion can reach.

## Background: why agents get nothing today

Three measured facts, not inferences.

1. **The hooks cannot fire.** `shell/undo.zsh:114` registers `preexec` and
   `precmd` through `add-zsh-hook`; the bash hook uses the `DEBUG` trap and
   `PROMPT_COMMAND`. None of these run for `<shell> -c '...'`. `UNDO_SESSION`
   is therefore never set, `armed()` returns false (`shim/undo_shim.c:72`), and
   nothing is recorded.

2. **The shell is not knowable in advance.** Agent harnesses run the user's
   login shell. Measured in one developer setup: `arg0` was `/bin/zsh`,
   `BASH_VERSION` unset, `ZSH_VERSION` present. A design keyed to `$BASH_ENV`
   captures nothing there. Per-shell hook points exist but differ in kind —
   bash has `$BASH_ENV` with `$BASH_EXECUTION_STRING`; zsh has `~/.zshenv`,
   always sourced, with `$ZSH_EXECUTION_STRING` empty for script invocations;
   tcsh sources `.cshrc` even non-interactively; `sh`/`dash` have nothing at
   all. Four mechanisms, one of them absent, is not a design.

3. **The plumbing already exists.** The shim arms on `UNDO_SESSION` being set
   and pointing at a directory. Nothing about that is interactive; only the
   thing that sets it is.

The conclusion is to arm through the environment alone — inherited, not
sourced — and move session creation into the shim. That removes the shell
dependency entirely, and with it the empty-session churn and the
session-creation race that a hook-based design has to work around.

## What an LD_PRELOAD shim can and cannot see

Verified by direct measurement rather than assumed, because the answer turned
out to be neither "yes" nor "no".

**Rust-based harnesses go through libc.** Their in-process file operations are
fully visible to the shim.

**Bun-based harnesses are visible for some calls and not others.** Bun links
glibc and dispatches some file operations through it while issuing others as
raw Linux syscalls. Measured against a shim build, on a current Bun:

| Operation | Reaches libc? | Journaled? |
|---|---|---|
| `fs.unlinkSync` | yes, via `unlinkat` | **yes** |
| `fs.rmSync` recursive | yes, via `unlinkat` + `AT_REMOVEDIR` | **yes** |
| `fs.truncateSync` | yes, via `truncate` | **yes** |
| `fs.writeFileSync` over an existing file | no | **no** |
| `fs.openSync` / `fs.appendFileSync` | no | **no** |
| `fs.renameSync` | no | **no** |
| `fs.copyFileSync` | no | **no** |
| `Bun.write` | no | **no** |
| read-then-write in place (the shape an edit tool uses) | no | **no** |

`strace` confirms the syscalls happen — `openat`, `renameat` — while no journal
record appears. Bun is issuing those directly and never touching the libc
symbols the shim interposes.

Two consequences.

**Arming a Bun-based harness process would capture its deletions and miss every
overwrite and rename** — that is, miss precisely the write and edit tools worth
covering. So harness-process arming is not the mechanism for in-process writes.

**And it would be fragile in the worst direction.** Coverage would depend on an
undocumented internal choice in a third-party runtime, free to change in any
release with no signal. A user believing they are protected and not being so is
the failure mode `undo` exists to eliminate.

In-process writes are therefore covered by a different mechanism — a capture
verb driven from the harness's own pre-tool hook, which runs as a subprocess
and does not care what the harness runtime does internally. That is a separate,
smaller design; see Phasing.

## Design

### 1. Arming

A process is armed when `LD_PRELOAD` contains `libundo.so` **and** `UNDO_ARM`
is set. The environment carries four things:

| Variable | Meaning |
|---|---|
| `LD_PRELOAD` | the shim, prepended |
| `UNDO_DATA_DIR` | the store; sessions live under `sessions/`, group links under `groups/` |
| `UNDO_ARM` | `<pgid>:<starttime>` of the arming process, or `1` |
| `UNDO_SID` | the terminal session id at arm time |

`UNDO_ARM` carries the armer's identity rather than a bare flag because two
tests depend on it: excluding the armer's own process group, and detecting a
process that has detached from it. `<starttime>` is field 22 of
`/proc/<pid>/stat`; without it a recycled pgid could match a long-dead armer
and silently disable capture.

**The exclusion test is exact, not a pgid comparison.** A process is excluded
only when its own pgid *and* that group leader's starttime both equal the pair
in `UNDO_ARM`. Comparing the pgid alone would let a recycled number match a
long-dead armer, and the symptom of that is silence.

The entry point is a new subcommand:

```
undo arm -- <program> [args...]
```

which resolves the shim, sets the four variables, and `exec`s. `exec` preserves
pid, pgid and sid, so the recorded identity is exactly the armed program's. It
also sets `UNDO_HOOK=arm`, so `undo doctor` can distinguish an inactive arming
from a missing install, as the shell hooks already do.

**Two documented arming sites, one mechanism.** Nothing in the shim branches on
which is used.

- **Agent-scoped** — `undo arm -- <agent>`, or the equivalent two lines in the
  agent's launcher. Blast radius is agent activity only. Requires no node-image
  change and no administrator sign-off, which is why it is what ships first.
- **Login-scoped** — `exec undo arm -- $SHELL` from the login environment, or
  the same variables set in a node image. Covers every process the user runs,
  including shells that have no hook at all. **Gated** on the administrative
  question in "Open questions" below; nothing here presumes the answer.

**The degraded static form.** A harness that can only set fixed environment
values may set `UNDO_ARM=1`. Lazy creation then works, but with no armer
identity there is no exclusion test and no detach test. This is a real
reduction in correctness, so `undo doctor` reports it explicitly rather than
letting it pass as normal.

### 2. Grouping: one session per process group

The unit of undo is the process group. In an agent harness each tool call gets
its own group — `pid == pgid`, distinct across calls, inherited by children —
so one group is one tool call, which is exactly the granularity a user asking
to "revert that command" means.

The group key is

```
<hostname>-<boot-id>-<pidns-inode>-<pgid>-<leader-starttime>-<age-bucket>
```

with every byte outside `[A-Za-z0-9._-]` replaced by `_` and the hostname
truncated to 64 bytes. The result stays comfortably inside `NAME_MAX`.

Each component earns its place:

- **hostname, boot id, pid-namespace inode** — the same three-part origin
  `internal/session/session.go:184` already composes, for the same reason. Home
  directories are shared across nodes, so two machines will both have pgid
  5000; a reboot reissues pgids; and containers sharing a kernel and a UTS
  namespace have separate pid namespaces where the same number names unrelated
  processes. A subset is not a weaker version of this, it is a wrong answer.
- **pgid** — the grouping itself.
- **leader starttime** — defeats pgid reuse within one boot.
- **age bucket** — `floor(elapsed_since_group_start / UNDO_SESSION_MAX_AGE)`.
  See "Bounding a long-lived group" below.

**A process that calls `setsid()` becomes its own group leader and gets its own
session.** This is the intended answer, not a wart: a program that deliberately
detaches should not keep writing into the tool call's undo history.

**Where the grouping fails, and how it fails.** In an environment that creates
no process groups at all — a container with no job control, where everything
shares pgid 1 — every process matches the armer's group and nothing is ever
captured. That fails closed: safe, but silently useless, which is why `undo
doctor` reports it (section 8).

### 3. Rendezvous: two processes, one session

Members of a group discover each other through an atomic symlink under
`groups/`. The ordering matters and is not the obvious one:

1. Generate a session id from the clock, in the same format `Create` produces —
   unix seconds then six digits of microseconds, which is what
   `session.sessionID` validates and what the orphan sweep depends on.
2. `mkdir(sessions/<id>)`. On `EEXIST`, bump and retry, exactly as `Create`
   does. **The mkdir is the arbiter for id collisions across unrelated groups**;
   without it two groups creating in the same microsecond would share one
   session and interleave their journals.
3. `symlink("../sessions/<id>", groups/<key>)`.
4. On success, finish setup. On `EEXIST`, another member of our own group won:
   `rmdir` our unused session directory, `readlink` the key, take the basename
   of what it returns, and use that id.

**The target is `../sessions/<id>` and not the bare id, which is not a
cosmetic choice.** A symlink target resolves relative to the link's *parent*,
so a bare `<id>` at `groups/<key>` would resolve to `groups/<id>` — a path
that never exists. Every group link would then be dangling, and the `undo gc`
prune below would delete the mapping of every *live* session on its first run.
Raised by the gate; the failure is total and silent, and the obvious
implementation is the broken one.

Symlink creation is atomic and is the classic primitive for exactly this on
network filesystems. There is no polling and no leader election beyond picking
the id.

**After the id is agreed, every member performs the same idempotent setup** —
`mkdir -p sessions/<id>/data`, then write `cmd`, `pid`, `pgid`, `ttl`, `host`.
All five describe the *group*, not the writer, so racing processes write
byte-identical content and the race is not merely tolerable but invisible.

A sixth file, `journalv`, is written by the shim before its first journal
record rather than here, because sessions created by a shell hook need it too
and the hook may be an older one. It is idempotent for the same reason. See
section 7.

Two residual cases, both bounded:

- If the `rmdir` in step 4 fails, an empty session directory leaks. It costs no
  retention slot and is collected once past the empty-session grace.
- If the session directory named by an existing symlink is missing — reachable
  only through `undo purge --force` on a live session — the shim unlinks the
  stale symlink and retries once. Otherwise it would append to a journal in a
  directory that does not exist and lose every subsequent record silently.

`groups/` is created `0700` and is pruned by `undo gc`, which removes symlinks
whose target session directory no longer exists. A dangling link is
unambiguous: the session directory is always created before the link.

### 4. What a shim-created session records

Beyond the existing `cmd`, `pid` and `host`:

| File | Contents | Why |
|---|---|---|
| `pgid` | the process group id | the group, not one pid, is what is alive |
| `ttl` | unix seconds at which this age bucket ends | makes "finished" computable without cooperation |

`ttl` holds an integer rather than a formatted timestamp so the shim needs no
time formatting and no new libc symbol.

`host` must be byte-identical to `composeHost` — hostname, tab, boot id, tab,
the raw `pid:[<inode>]` link target — or a session reads as foreign on the node
that created it, is pinned for the whole grace, and `undo` refuses to revert
it.

`cmd` comes from `/proc/<pgid>/cmdline` with NULs turned into spaces, falling
back to `/proc/self/cmdline` when the leader has already exited. For an agent
tool call the group leader *is* `<shell> -c '<the whole command>'`, so the
pipeline survives intact — the entire command is one argv element. The
often-cited objection that a leader's cmdline loses pipeline detail applies to
an interactive shell, where the leader is one member of the pipeline, and not
to the case this feature exists to serve.

`pid` still records the leader's pid so an older `undo` binary keeps working
against a newer session.

### 5. Liveness, and bounding a long-lived group

**`probe()` gains a group arm.** When a session records a `pgid` greater than
1, liveness is `kill(-pgid, 0)` rather than `kill(pid, 0)`. Probing the leader
is wrong the moment the leader exits while a child keeps writing — which is GC
deleting a running command's backups, the defect the cross-node liveness work
already fixed for a different cause.

The `> 1` guard is load-bearing. `kill(-1, 0)` is the "every process you may
signal" form; it would succeed always and pin the session forever.

Sessions with no `pgid` file keep today's behaviour exactly, so a rollout
strands nothing already on disk.

**The full liveness rule**, stated in order because the interaction with the
existing foreign-origin path is otherwise open to two readings:

1. A `done` marker is conclusive: finished.
2. **A recorded `ttl` is authoritative from any node.** Past `ttl + ttlSkew`
   the session is finished regardless of origin. Unlike a pid, `ttl` names an
   absolute instant, so it means the same thing read from anywhere — which is
   what makes the long-lived-group bound work even when `undo gc` only ever
   runs on some other node.
3. A session from another origin cannot be probed. Before its `ttl` it is
   presumed running; with no `ttl` at all it falls back to `foreignGrace`,
   exactly as today.
4. Otherwise probe: `kill(-pgid, 0)` when `pgid > 1`, else `kill(pid, 0)`.

`ttlSkew` is five minutes, because `ttl` is written by one node's clock and may
be read by another.

Step 2 is what retires a rolled bucket. An earlier revision had
the rolling process write `done` into the bucket it left. That was dropped:
it depends on *someone noticing*, and a process that goes quiet across a
boundary and never calls again would leave a finished session live for the
entire remaining life of the group — which for a daemon is unbounded, and a
session that reads live is a session GC provably cannot collect. `ttl` needs no
cooperation from anyone.

**The roll.** Each armed process caches its resolved key and session directory.
On a later destructive call it recomputes the bucket; a change means
re-resolving through the rendezvous. Nothing else happens, because the previous
bucket has already retired itself.

**Why bound at all.** A daemon, a terminal multiplexer pane, or any process
that inherits the environment and outlives its tool call would otherwise hold
*one session that grows without limit* — and, because its group leader stays
alive, one that `Live()` never releases and GC therefore never collects. That
is a storage leak on shared quota with no escape short of `undo purge --force`.

`UNDO_SESSION_MAX_AGE` is in seconds, defaults to 21600 (six hours), and is
floored at 60 so a mistuned value cannot make every call its own session. Because
the bucket is measured from the group's own start rather than wall-clock
boundaries, an ordinary tool call lasting seconds never splits. A long-lived
group produces four sessions a day, each bounded and each independently
collectible — few enough not to evict genuine sessions out of `UNDO_KEEP`, and
short enough that no single journal accumulates a day of writes.

**Idleness costs nothing.** Creation is lazy, on the first destructive call. A
login shell that sits there, or an agent between tool calls, creates no
session, no journal, no store directory, and occupies no retention slot.

### 6. The detach test

A process that inherits the arming environment and then `setsid()`s away —
a terminal multiplexer server, a screen daemon, any properly daemonizing
program — carries an inherited `UNDO_SESSION` with it for its entire life, and
hands it to every child it later spawns.

The consequence is not a degradation. Those children append journal records to
a session that finished long ago, which GC will eventually delete; after that
they append to an *unlinked inode*, and nothing is printed. That is the same
end state as the cross-node liveness defect, reached by a different route. It
is latent today wherever `LD_PRELOAD` is exported to children; arming through
the environment makes it the normal case.

**The test:** whoever sets `UNDO_SESSION` also exports `UNDO_SID`, the terminal
session id at that moment. The shim rejects an inherited `UNDO_SESSION` when
`UNDO_SID` is set and `getsid(0)` differs from it, and falls through to lazy
creation.

It works because job control changes a child's *pgid* but not its *sid*. An
ordinary command from a hooked shell still matches; anything that daemonized
does not.

It is deliberately conservative in one direction: **with `UNDO_SID` unset there
is no trustworthy reference, so `UNDO_SESSION` is honoured exactly as today.**
That is what keeps sessions created by older hooks working through a rollout,
and what stops the test from silently disarming a shell whose hook has not been
updated.

The three shell hooks are updated to export `UNDO_SID`, which closes the
pre-existing leak rather than routing around it. A shell can read its session
id without a subprocess: field 6 of `/proc/self/stat`, once at source time.
`getsid` has been in glibc since 2.2.5 and does not move the symbol floor.

### 7. Journal integrity

**`jwritev` discards `write()`'s return value** (`shim/undo_shim.c:118-120`).
This is a wrong-restore bug, not a lost-record bug. A short write leaves a
record with no terminating newline, so the next record concatenates onto it:

```
unlink<TAB>/path/to/fi          (truncated, no newline)
unlink<TAB>/other<TAB>/bak<TAB>link
```

is read as one entry whose path is `/path/to/fiunlink` and whose backup is
`/other`, because `journal.Read` splits on tabs and validates nothing beyond
having two fields (`internal/journal/journal.go:169`). Restore then writes the
wrong backup to the wrong path, silently.

Short writes to a regular file arise from `ENOSPC` and `EDQUOT`, which is to
say from ordinary quota exhaustion on shared storage — reachable, not
theoretical.

**The writer-side fix alone is not sufficient, and this is the important
part.** Checking the return and emitting a lone terminating newline closes the
single-writer case. Under *concurrent* writers in one process group it does
not: the newline is a second `write()`, so another member can append a complete
record between the short write and the newline. That record then merges with
the damaged prefix and the corruption is exactly as before. Raised by the gate,
and correct — concurrent writers in one group is not an exotic case, it is
`make -j`, `xargs -P`, or any parallel test suite inside one tool call.

The deeper problem is that **every writer-side remedy asks a failing writer to
perform more successful I/O at the moment I/O is failing.** The newline write
can itself fail on the same quota that truncated the record.

**So the guarantee is reader-side: every record carries a trailing integrity
field.** The shim appends `~<16 hex digits>`, an FNV-1a hash of the encoded
record, computed during the pass it already makes to percent-encode. The reader
recomputes it over everything preceding the final tab and rejects any record
that does not match.

This is decisive against the merge because a merged line carries the *second*
record's hash over the *first* record's prefix, so it can never validate. It
also catches damage the writer never noticed — a truncated read, a partially
replicated file — rather than only the case the writer detected.

Three properties that make it safe to add:

- **Additive, as the format requires.** It is a trailing field, and
  `journal.Read` already tolerates trailing fields. Every field access in
  `journal.go`, `restore.go` and `session.go` is bounds-guarded; none assumes
  an exact field count. Verified, not assumed.
- **Whether validation is required is declared per journal, not inferred per
  record.** The shim writes a `journalv` file into the session directory,
  idempotently, before its first journal record. Its presence means *every*
  record in that journal must validate; its absence means legacy rules.

  Inferring it per record — "no trailing marker means legacy" — is the obvious
  rule and it is wrong, which the gate caught. A short write that truncates a
  new record *before* its integrity field leaves a line with no marker, which
  that rule accepts as legacy; if enough fields survived to satisfy the parser,
  restore acts on a partial path. That is precisely the single-writer failure
  the field exists to close, reintroduced by the compatibility rule.

  A truncated record cannot erase `journalv`: it is a different file, written
  once at session setup rather than per record. The residual hole is only
  "`journalv` could not be written at all", which is a creation-time failure
  and degrades to today's behaviour rather than below it.

  Consequence worth stating: a journal that somehow mixes records from an old
  and a new shim — two members of one group with different shim builds — has
  `journalv`, so the old shim's records fail validation and are rejected.
  Over-strict, and closed in the safe direction.
- **Old readers are unaffected.** The field is trailing and `journal.Read`
  already tolerates trailing fields, so an older `undo` binary reading a newer
  journal ignores both the field and `journalv` and behaves exactly as it does
  today. This is why the marker is appended rather than prepended: a leading
  sentinel would be airtight against truncation, but it would shift every field
  index, and an old binary would then parse every op as unknown — listing a
  session as having N changes and restoring none of them.
- **Rejection is not filtering.** A rejected record is corrupt, not merely
  unrecognised. Journal indices are load-bearing — `restore.slot()` and the
  interactive picker are keyed by position — so a rejected record must still
  occupy its slot, marked unrestorable, rather than being dropped and shifting
  every index after it.

The newline-on-short-write stays as cheap containment for the single-writer
case; it is no longer what the correctness rests on.

**Concurrency otherwise answers itself structurally.** Parallel agent tool calls each get
their own pgid, hence their own session, hence their own journal; they never
share one. The only concurrent writers to a single journal are members of one
process group, which by definition live on one node and so behind one network
filesystem client, where the client serialises appends through the inode. The
guarantee that is genuinely absent — atomic append across *clients* — cannot be
reached, because a process group cannot span clients.

This is worth stating rather than assuming, because it is the property that
makes the whole design safe under an agent running many tool calls at once.

### 8. Blast radius, ignores, and diagnostics

Arming an agent means its builds, test suites and package installs are
journaled too. The existing mechanisms already bound this: `mod_seen` dedup
means one backup per path per session, `UNDO_MAX_BYTES` caps each copy,
`UNDO_MAX_STORE` and `UNDO_KEEP` bound the whole store, and `create` records
take no backup at all.

**`default_ignores` is deliberately not expanded.** It is shared with upstream
and kept minimal, and silently ignoring more paths is how a user comes to
believe they are protected when they are not. Instead this design ships a
*documented recommended* `UNDO_IGNORE` for agent arming, applied by the user,
with `undo doctor` printing the effective list.

One argument for using it is not noise but secrecy: an armed agent that
rewrites its own credential or transcript files copies them into the store.
The store is `0700`, but it is still a second copy in a second place, and on
shared storage that deserves to be a decision rather than a side effect.

**`undo doctor` gains an arming section**, because the silent failure mode of
this design is *everything shares the armer's process group, so nothing is ever
captured*. Doctor reports:

- whether `LD_PRELOAD` names the shim and `UNDO_ARM` is set;
- whether `UNDO_ARM` carries an identity or is the degraded `1` form, and
  therefore whether the exclusion and detach tests are active;
- the group key this process resolves to, and whether its group is the armer's
  own — if it is, capture is disabled here;
- the effective ignore list.

## Invariants

Inherited unchanged from the companion design, and each newly stressed:

1. **The shim must never cause the user's command to fail.** This design adds
   several new failure paths inside the shim — a `mkdir`, a `symlink`, a
   `readlink`, several small writes, and `/proc` reads — every one of which is
   a new opportunity to violate it. All errors are swallowed and the real
   syscall's result returned untouched. Asserted by test with the store
   unwritable.
2. **No new libc call may raise the glibc symbol floor above `GLIBC_2.34`.**
   The calls this design adds — `getsid`, `symlink`, `readlink`, `gethostname`,
   `clock_gettime` — all predate that floor. Re-verified by `objdump` after any
   shim change, not assumed.
3. **No site-specific data in this repository.**

## Degradation ladder

Every failure falls back to something safe, and every irreversible degradation
is reported.

| Failure | Falls back to | Visible? |
|---|---|---|
| `UNDO_ARM` unset | no capture; behaviour identical to today | doctor |
| Process group equals the armer's | no capture | **yes, doctor** |
| `UNDO_ARM=1` static form | no exclusion, no detach test | **yes, doctor** |
| `groups/` or `sessions/` unwritable | no capture; syscall result untouched | doctor |
| `/proc/<pgid>/stat` unreadable | `starttime` recorded as 0; pgid reuse may merge two commands into one session | `undo show` |
| Leader exited before first destructive call | `cmd` from `/proc/self/cmdline` | `undo show` |
| Stale group symlink, session gone | symlink unlinked, one retry | — |
| `rmdir` of an unused session directory fails | empty session leaks, costs no retention slot | — |
| Short journal write | record terminated, next record clean | — |
| Record fails its integrity check | slot kept but marked unrestorable, never acted on | **yes, loudly** |
| Session has no `journalv` | legacy rules, no validation | — |
| `journalv` unwritable at session setup | legacy rules; degrades to today, not below | doctor |
| Long-lived group | one session per `UNDO_SESSION_MAX_AGE` | `undo list` |
| Bun-based harness, in-process write | not captured at all | **documented limit** |

## Testing

Upstream's `CONTRIBUTING.md` requires an end-to-end case for any shim change.
Beyond that:

- **A simulated tool call** — a command run in its own process group with only
  the environment set — creates a session, journals its changes, and becomes
  collectible once the group exits.
- **Rendezvous:** two processes in one group racing to create yield exactly one
  session, and both journal into it.
- **pgid reuse:** same pgid, different leader `starttime`, yields two sessions.
- **Detach:** a process that `setsid()`s ignores an inherited `UNDO_SESSION`
  and creates its own session; with `UNDO_SID` unset it honours the inherited
  one, so a rollout does not disarm anyone.
- **Group liveness:** a leader that exits while a child keeps writing still
  reads live, and GC does not collect it. This is the regression that a pid
  probe would silently reintroduce.
- **The roll:** with a small `UNDO_SESSION_MAX_AGE`, a long-lived group
  produces a second session, and the first is retired by `ttl` with nobody
  having written `done`. Retirement is asserted against a *fabricated* past
  `ttl`, not by waiting: `ttlSkew` is five minutes, and a test that sleeps
  through it is a test nobody runs.
- **`ttl` is authoritative cross-node:** a session whose origin is foreign and
  whose `ttl` has passed is collectible, even though `foreignGrace` would still
  be holding it.
- **Invariant 1 under the new paths:** with `sessions/` and `groups/`
  unwritable, every interposed call still returns the real syscall's result and
  the real `errno`.
- **Journal short write:** a truncated record does not merge with the next one.
- **Integrity field:** a hand-built journal in which a truncated record is
  followed by a complete one — the concurrent-writer merge, constructed
  directly rather than raced — is rejected rather than restored. This is the
  case the writer-side newline cannot cover, so it is the one that must be
  asserted.
- **Legacy journals:** a session with no `journalv` restores exactly as it does
  today, integrity fields absent throughout.
- **The downgrade hole stays closed:** a session *with* `journalv` whose last
  record was truncated before its integrity field is rejected, not accepted as
  legacy. This is the case a per-record inference rule would have passed.
- **Indices survive rejection:** a journal with one corrupt record in the middle
  leaves every later record at the same slot number the interactive picker and
  `restore.slot()` would have given it before.
- **glibc floor** re-asserted by `objdump` after the shim change.

## Phasing

1. **Shim-side capture** — this document. Arming, grouping, rendezvous, session
   lifecycle, the detach test, the journal short-write fix, doctor.
2. **In-process capture** — an `undo capture <path>...` verb invoked from the
   harness's pre-tool hook, covering the write and edit tools that never spawn
   a subprocess and are invisible to `LD_PRELOAD` on a Bun-based runtime. It
   depends on the session model established here, and is otherwise independent.
3. **Login-scoped arming** — the same mechanism, a different arming site,
   gated on the administrative question below.

## Non-goals

- **Static binaries and programs making raw syscalls**, including Go binaries
  and the raw-syscall paths of Bun-based runtimes. An architectural limit of
  `LD_PRELOAD`. Documented in the coverage table above rather than worked
  around, because a partial answer here is worse than an honest gap.
- **Per-command granularity inside a long-lived unhooked shell.** A
  multiplexer pane gets one age-bounded session covering many unrelated
  commands. `undo -i` picks individual entries out of it. Finer granularity
  there is what the shell hooks are for.
- **Cross-client atomic journal append.** Structurally unreachable, and
  structurally unnecessary; see section 7.

## Open questions

These need answers from cluster and storage administrators. Nothing above
designs around an invented one.

1. **Snapshot policy per volume.** Drives the per-volume budgets, and therefore
   how much an armed agent may keep.
2. **Distributed-filesystem metadata load and subtree quota accounting for
   hardlinked backups.** Unchanged from the companion design.
3. **Whether loading the shim into every process a user runs is acceptable to
   whoever owns the node images.** This gates the *login-scoped* arming site
   only. Agent-scoped arming needs no node-image change and no sign-off, which
   is why phase 1 ships that way.
4. **Metadata traffic from per-tool-call session creation.** Each destructive
   tool call creates a session directory, six small files and one symlink on
   the shared home filesystem. Lazy creation means read-only tool calls cost
   nothing, which removes most of the volume, but an agent doing hundreds of
   destructive calls is a metadata pattern worth sizing before wide
   deployment. New with this design.
