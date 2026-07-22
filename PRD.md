# undo — PRD

## Problem

You run a command, it does something you didn't mean: `rm -rf` the wrong
directory, `>` over a file you needed, `mv` on top of something important.
The shell has no undo. Trash cans only cover graphical file managers, and
filesystem snapshots (btrfs, zfs) are coarse, machine-wide, and not tied to
"that one command I just ran".

## Idea

`undo` reverts the filesystem changes made by the previous shell command.

```
$ rm -rf notes/
$ undo
restored 132 files, 9 directories (rm -rf notes/)
```

## How it works

1. **Shim** (`libundo.so`, C): an `LD_PRELOAD` library. When armed via
   `UNDO_SESSION`, it intercepts destructive libc calls (`unlink`, `rename`,
   `open` with `O_TRUNC`/`O_CREAT`, `rmdir`, `mkdir`, `truncate`, ...).
   Before the real call runs it saves the affected file into the session's
   backup area (hardlink when possible, copy otherwise) and appends a line
   to a journal. Deleting a file becomes "move a link", so even `rm -rf` on
   large trees is cheap.

2. **Shell hook** (`undo.zsh`): a `preexec` hook creates a fresh session
   directory per command and exports `UNDO_SESSION` + `LD_PRELOAD`; a
   `precmd` hook disarms them, deletes sessions that recorded no changes,
   and prunes old sessions.

3. **CLI** (`undo`, Go): reads a session journal and replays it in reverse:
   relink deleted files, rename moved files back, restore truncated
   contents, remove created files, recreate removed directories.

## Commands

- `undo` — revert the most recent not-yet-undone session (asks first).
- `undo list` — recent sessions: id, command, change count.
- `undo show [id]` — what a session changed.
- `undo apply <id>` — revert a specific session.
- Flags: `-n/--dry-run`, `-y/--yes`, `--force` (clobber conflicts).

## Non-goals / known limits (v1)

- Only commands run through the hooked shell are covered.
- `LD_PRELOAD` doesn't reach static binaries, programs doing raw syscalls
  (Go binaries), or setuid programs (`sudo` strips it). Coreutils are
  glibc-dynamic, so the classic footguns (`rm`, `mv`, `cp`, shell `>`) work.
- No redo. No chmod/chown/ftruncate tracking. Linux only.
