# undo (multi-filesystem fork) — agent instructions

`undo` is an `LD_PRELOAD` shim (`shim/undo_shim.c`, C) that journals destructive
libc calls, plus a Go CLI (`cmd/undo/`, `internal/`) that replays that journal in
reverse. This is a **public fork** of `edaywalid/undo` carrying multi-filesystem
support: one backup store per filesystem instead of one per machine. A change
here usually touches the shim, `internal/restore/restore.go`, or
`internal/session/session.go`. The highest-risk surface is the shim: it is loaded
into every process the user runs, so a bug there breaks unrelated commands.

## Build and verify

```bash
test/in-container.sh make test                    # unit + e2e
test/in-container.sh --privileged test/multifs.sh # two-filesystem harness
tools/check-no-site-data.sh <files...>            # or: git ls-files -z | xargs -0 ...
tools/check-ere.sh                                # scanner patterns must compile
```

**Baseline verified 2026-08-02 at `c39702f`: fully green on aarch64.** `go test
./...` passes and `test/e2e.sh` reports `all cases passed` across all 52 e2e
cases, run in a bookworm container on the Apple Silicon workstation. amd64
coverage comes from CI only (the el9 job). There are no known-failing tests —
if a test is red, you broke it.

## Facts that override default assumptions

- **The tool is Linux-only; the workstation is macOS.** Never run `make`, `go
  test`, `gcc`, or `test/e2e.sh` directly — they will fail or, worse, silently
  test the wrong platform. Everything goes through `test/in-container.sh`.
  `test/multifs.sh` additionally needs `--privileged` because it mounts tmpfs.
- **`test/in-container.sh` inherits the host architecture — aarch64 on an Apple
  Silicon workstation — while the deployment target is el9/x86_64.** Emulating
  amd64 (`--platform linux/amd64`) is **not** a valid substitute: under qemu
  user-mode emulation `/proc/self/stat` field 6 (session id) reads 0 while
  `getsid(2)` correctly reports the real id (measured: bookworm/amd64 emulated
  and rocky9/amd64 emulated both read field 6 as 0, getsid(0) as 1). A
  /proc-derived `UNDO_SID=0` silently switches off the shim's detach guard,
  so an emulated green run proves nothing. Real amd64 coverage exists only in
  the el9 CI job; `test/e2e.sh` refuses to run when it cannot resolve a
  trustworthy session id.
- **The shim must never cause the user's command to fail.** All internal errors
  are swallowed and the real syscall's return value is passed through untouched.
  This is not a style preference; it is the property that makes the tool safe to
  preload globally. It is asserted by test.
- **No new libc call may raise the glibc symbol floor above `GLIBC_2.34`.** The
  build target is el9/x86_64. This is why `parse_ulong` is hand-rolled instead of
  calling `strtoul` — under `_GNU_SOURCE` a modern glibc redirects `strtoul` to
  `__isoc23_strtoul`, which needs 2.38 and makes the `.so` refuse to load. After
  any shim change, verify:
  ```bash
  test/in-container.sh bash -c '
    gcc -shared -fPIC -O2 -Wall -o /tmp/libundo.so shim/undo_shim.c -ldl
    objdump -T /tmp/libundo.so | grep -o "GLIBC_[0-9.]*" | sort -u -V | tail -1'
  ```
- **No site-specific data in this repository, ever.** No hostnames, mount points,
  organization domains, internal addresses, usernames, or storage capacities.
  Deployment values live in a separate private repo. `tools/check-no-site-data.sh`
  enforces this in a pre-push hook and in CI. **Nothing is exempt** —
  `.check-no-site-data-ignore` is deliberately empty and must stay that way; write
  test fixtures as concatenated fragments that individually match nothing rather
  than requesting an exemption.
- **Upstream's `CONTRIBUTING.md` requires an e2e case for any shim change.**
  Append to `test/e2e.sh`; cases are numbered sequentially.
- **The journal format is append-only and additive.** Lines are
  `op<TAB>field<TAB>...` with `%`, control bytes and DEL percent-encoded.
  `journal.Read` tolerates trailing fields, so new fields may be *appended* to an
  op — never inserted, never reordered. Journal indices are load-bearing:
  `restore.slot()` and `--only` are keyed by position, so records must never be
  filtered out of the parsed entry list.
- **`make` installs the git hooks** by setting `core.hooksPath`, which git stores
  per clone and therefore loses on every fresh clone. Run `make` before pushing.
- **The Go module path is `github.com/edaywalid/undo` (upstream's).** Do not
  rename it; the fork tracks upstream and rebases onto it.
- **Fork-local files must never reach an upstream pull request.** `AGENTS.md`,
  `docs/`, `test/in-container.sh`, `test/multifs.sh`, and `tools/` are fork-local.
  PR branches are cut from `upstream/main` and carry only the fix.
- **macOS ships bash 3.2.** Scripts run from the workstation cannot use `mapfile`
  or a bare `"${arr[@]}"` under `set -u`.

## Review priorities for this repo

1. **Any new way the shim can change the outcome of the user's syscall.** A new
   failure path, an early return that skips the real call, a directory created
   where it makes an `rmdir` fail with `ENOTEMPTY`, a clobbered `errno`, or a
   reentrancy guard that is not cleared on every path. This class has already
   produced one silent, unrecoverable bug in this codebase.
2. **Silent data loss that looks like success.** A backup recorded in the journal
   but unreachable, deleted, or pointing at the wrong path; a hardlinked backup
   counted at full size and evicted; an `undo` that reports "restored N changes"
   having restored nothing. Every check here passes while the tool is useless,
   which is exactly the failure mode `undo` exists to prevent.
3. **Deletion outside the store.** GC, `purge`, and `Session.Remove` derive paths
   from journal contents. A path must be proven to lie inside a store directory
   before anything removes it.
4. Raised glibc floor, or site-identifying content.
5. Violations of the approved plan.

## Multi-agent workflow

Roles:

- **Orchestrator** (Claude Code) — brainstorms, writes the spec and plan, then
  dispatches, adjudicates reviews, and integrates. Does not implement
  substantive code itself.
- **Implementer** (`opencode run --agent implementer`) — implements an
  already-approved plan; follows it rather than redesigning, and stops to report
  if the plan is wrong or ambiguous. Trusts the facts and commands in this file.
- **First-pass reviewer** (`opencode run --agent reviewer`) — read-only
  adversarial pass applying the review priorities above.
- **Adversarial gate** (`codex exec review`) — the routine gate. `--commit <sha>`
  reviews one commit, `--base <branch>` a whole branch, `--uncommitted` the
  working tree.

Single source of truth: if a fact about this repo matters to any agent, it
belongs in this file, not baked into an agent's prompt.

Portable lessons:

- **Verification-first.** The highest-yield check is executable — run it, on real
  data. Weight the pipeline there, not on review volume.
- **Adjudicate review findings against ground truth; never auto-apply.** The
  first-pass reviewer and the codex gate both produce confident false positives.
- **Reasoning is opt-in for the implementer.** `--variant none` when the dispatch
  already contains the exact code and structure; `--variant max` when the
  approach itself must be worked out. Skip the middle.
- **`opencode run` can block indefinitely on a network call**, having printed
  only its short header. Always dispatch through `ocrun`, which declares a stall
  after N seconds of zero new output. If one stalls, apply the approved plan
  directly and still run the codex gate — the gate must not be skipped.
- **Never put a pipeline on the end of an `ocrun` dispatch.** `ocrun ... | tail`
  reports `tail`'s exit status, hiding `ocrun`'s 124 (stall) or 125 (max wall
  time), so a hung provider reads as a clean run that produced nothing. Check
  `$OCRUN_DIR` instead: `sample.txt`, `sockets.txt` and `opencode.log.tail` are
  written only when `ocrun` kills something, and a trailing
  `message=stream providerID=... modelID=...` in the log with nothing after it
  means the provider never answered — retry on a different agent/model rather
  than rewriting the prompt.
- **Always end a `codex exec` dispatch with `< /dev/null`.** It treats non-TTY
  stdin as additional prompt input and blocks until EOF, so a backgrounded run
  hangs on an open pipe and is eventually killed having produced nothing. The
  only clue is its first line, `Reading additional input from stdin...`, which a
  trailing pipeline would also swallow. When any dispatch dies with no output,
  read the output file before re-running it — twice in a row is a diagnosis, not
  flakiness.
