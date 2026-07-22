# undo — PRD

## Problem

You run a command, it does something you didn't mean: `rm -rf` the wrong
directory, `>` over a file you needed, `mv` on top of something important,
a build script that wipes a folder it shouldn't. The shell has no undo.

The accepted wisdom online is that this is unfixable after the fact:
"once data is overwritten by mv or redirection, recovery is essentially
impossible." A whole genre of "never run these 10 commands" articles
exists because the fear is real and the tooling answer is "be careful".

## Insight

The unit of regret is not a file, it is a command. People don't think
"I want `report.txt` back from the trash", they think "undo what that
last command just did". No existing tool models that.

Every current mitigation also asks the user to change behavior *before*
the accident (type `trash` instead of `rm`, set up btrfs snapshots,
configure backups). Accidents happen precisely when you didn't prepare.
`undo` requires no behavior change: you keep typing `rm`, and the safety
net is already under you.

## Positioning

One line: **Ctrl+Z for your terminal.** Revert exactly what the last
command did to the filesystem, on any Linux, any filesystem, no root,
no daemon, no new habits.

## Competitive landscape

**1. Trash-can rm replacements** — trash-cli (Python, the incumbent),
trashy (Rust), gtrash (Go, TUI restore), rip (Rust, /tmp graveyard),
trash-d, safe-rm, delayed_rm.

- Cover deletion only. Nothing for mv clobbers, `>` truncation,
  in-place overwrites, or accidental creation.
- Require replacing `rm` with another command. The community itself
  warns against aliasing rm (muscle memory betrays you on other
  machines, and aliases break scripts), so adoption fights consensus.
- Blind to deletions made *inside* programs: `npm run clean`, a
  Makefile, `git clean`, a misbehaving script. That is where the worst
  accidents live.
- Their feature race (gtrash's own comparison matrix) is entirely about
  restore UX: TUI, group restore, size summaries. Lesson: restore UX is
  what users compare.

**2. libtrash** — LD_PRELOAD library intercepting unlink/open/rename
into a trash can. Our closest technical relative, and proof the shim
mechanism is viable long-term (born 2002, still packaged).

- It is a global trash can, not an undo: no per-command sessions, no
  journal of what a command did, no rename reversal, no way to say
  "revert that". You dig files out by hand.
- Dormant (last release 2021), configuration-heavy, no modern CLI.

**3. Filesystem snapshots** — snapper, timeshift, zfs-auto-snapshot,
NILFS2, with httm as the restore UI.

- Real protection but needs btrfs/zfs/nilfs2 plus root setup. The
  default-ext4 majority has nothing.
- Time-granular, not command-granular: rolling back to "15 minutes ago"
  also reverts the work you wanted to keep, and the accident has to fall
  on the right side of the snapshot interval.
- httm's popularity shows appetite for file-level restore UX; it still
  cannot undo "what that command touched" as a unit.

**4. Inverse-command generators** — clvv/undo (by the fasd author).
Generates reverse commands (`mv B A` for `mv A B`). Fundamentally cannot
undo destructive operations because the data is gone; abandoned. We keep
the data, so we can.

## What makes us shine

1. **Command-granular undo.** Sessions map one-to-one to commands. The
   demo is one line: `rm -rf docs/` then `undo`. Nobody else has this.
2. **Coverage beyond deletion.** mv clobbers, `>` truncation, in-place
   rewrites, accidental creates, wrong-directory extractions. Every
   competitor covers at most the first category.
3. **Zero habit change.** Keep typing `rm`. Works when the deletion
   happens three processes deep inside a build script.
4. **Runs anywhere.** Any filesystem, no root, no daemon, no snapshots.
   Deletions are backed up by hardlink, so even `rm -rf` on huge trees
   is near-free.
5. **Honest scope.** A safety net for the last N commands, not a backup
   system. We say so loudly; trust is a feature.

## Shipped (v1)

- C LD_PRELOAD shim journaling unlink/rename/open/rmdir/mkdir/symlink/
  truncate with hardlink-or-copy backups, collision-safe, ~zero overhead
  when disarmed.
- zsh preexec/precmd hooks: one session per command, auto-pruning,
  empty-session cleanup.
- Go CLI: `undo`, `list`, `show`, `apply`, dry-run, confirmation,
  conflict detection (`--force` to override), one-shot undo semantics.
- End-to-end test suite covering rm -rf, clobber, mv, creation, symlink,
  dry-run, conflicts.

## Roadmap

**v1.x — make restore delightful (the axis users compare on)**

- `undo -i`: fzf-style TUI to browse sessions and cherry-pick entries to
  restore (selective restore, group restore). gtrash proved this is the
  killer restore UX; ours works across operation types, not just rm.
- `undo diff [id]`: show content diffs against the backups before
  restoring. Turns "trust me" into "see for yourself".
- `undo run -- <cmd>`: arm a session around a single command without any
  shell hook. Gives bash/fish/CI users the core value instantly and
  doubles as "run this sketchy script with a seatbelt".
- Redo: undoing writes an inverse journal, so `undo redo` reapplies.
- bash and fish hooks (DEBUG trap + PROMPT_COMMAND; fish preexec
  events). Triples the audience.

**v2 — from reactive to proactive**

- Guarded paths: `undo guard ~/thesis` makes the shim *refuse*
  destructive ops on protected trees (libtrash had a crude version;
  per-path allow/deny with a clear error is better than any rm alias).
- Noise control: config to skip journaling churn dirs (~/.cache,
  browser profiles, build dirs), plus per-process dedup so compilers
  rewriting the same file don't trigger repeated copies.
- Reflink backups (FICLONE) on btrfs/xfs: instant copy-on-write backups
  of big files, making in-place-write coverage free where the fs allows.
- `undo stats`: "undo has caught 1.2 GB of would-be-lost files". Fun,
  and it markets itself in screenshots.
- Pin sessions: `undo keep [id]` exempts a session from pruning.

## Distribution

- AUR package first (home turf), then Homebrew tap for Linuxbrew, deb/
  rpm via goreleaser+nfpm, one-line install script.
- README opens with an asciinema GIF of `rm -rf` followed by `undo`.
  The whole pitch fits in 5 seconds of footage.
- Show HN / r/commandline / r/linux launch once `undo run` lands, since
  that removes the "zsh only" objection from every comment thread.

## Risks

- **LD_PRELOAD reach.** Static binaries, raw-syscall programs (Go),
  and setuid/sudo are invisible. Mitigation: document loudly; coreutils
  and shells are glibc-dynamic, which is where the accidents are.
- **musl/non-glibc systems** (Alpine): shim untested there; needs CI.
- **Write-heavy workloads**: `mod` backups copy on every write-open;
  builds could feel it. Mitigation: noise-control config, dedup,
  reflinks, UNDO_MAX_BYTES cap (all on the roadmap).
- **Name collision**: undo.io (time-travel debugger company) and
  clvv/undo exist. Candidates if it ever matters: `undofs`, `oops`,
  `mulligan`, `rew`. Decide before wide publication.
- **Trust**: a tool that touches your files during a panic moment must
  never make things worse. Conflict detection, dry-run default in -i
  mode, and refusing double-undo are non-negotiable invariants.

## Success criteria

- A stranger installs it, wrecks a directory on purpose, gets it back,
  and the README never had to explain itself twice.
- The demo GIF is self-explanatory with the sound off.
- Zero reports of undo making a bad situation worse.
