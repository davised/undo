# undo

Undo what the last shell command did to the filesystem.

```
$ rm -rf docs/
$ undo
$ rm -rf docs/  (Jul 22 12:05:32, 4 changes)
  deleted   /home/you/project/docs/report.txt
  deleted   /home/you/project/docs/sub/note.txt
  deleted   /home/you/project/docs/sub/
  deleted   /home/you/project/docs/

revert this? [y/N] y
restored 4 change(s)
```

Works for the classic mistakes: `rm` / `rm -rf`, `mv` over a file you
needed, truncating with `>`, files and directories created by accident.

## How it works

There is no snapshotting and no daemon. A zsh hook arms a small
`LD_PRELOAD` library around every command you run. While armed, the
library intercepts destructive libc calls (`unlink`, `rename`, `open`
with write flags, `rmdir`, ...) and, before letting them through, saves
the affected file into a per-command session directory and appends a
journal line. Deleted files are saved by hardlink, so `rm -rf` on a huge
tree costs almost nothing extra. `undo` then replays the journal of the
last session in reverse.

Sessions live under `~/.local/share/undo/sessions/`, one per command
that actually changed something. The last 30 are kept.

## Install

```
make install          # installs to ~/.local
echo 'source ~/.local/share/undo/undo.zsh' >> ~/.zshrc
```

Requires: Linux, zsh, gcc, go.

## Usage

```
undo              revert the most recent command that changed files
undo list         recent sessions
undo show [id]    what a session changed
undo apply <id>   revert a specific session
```

Flags: `-n` dry run, `-y` skip confirmation, `--force` overwrite files
that changed again after the session.

`undo` refuses to clobber a path that was recreated after the session
unless you pass `--force`, and a session can only be undone once.

## Configuration

Environment variables, set before sourcing `undo.zsh`:

- `UNDO_KEEP` - sessions to keep (default 30)
- `UNDO_DATA_DIR` - session store (default `~/.local/share/undo`)
- `UNDO_MAX_BYTES` - largest file the shim will copy as a backup
  (default 256 MiB; only matters for in-place writes, deletions are
  hardlinked and have no size limit)
- `UNDO_CAPTURE_SHELL=1` - re-exec zsh once at startup with the shim
  preloaded, so redirections performed by the shell itself
  (`echo x > file`) are captured too. Without it only child processes
  are covered, which handles `rm`, `mv`, `cp`, `tee`, editors, etc.

## What it cannot catch

`LD_PRELOAD` only reaches dynamically linked programs that go through
libc:

- static binaries and programs that make raw syscalls (Go programs)
- setuid programs; `sudo` strips `LD_PRELOAD` entirely
- writes through already-open file descriptors (`ftruncate`), mmap
  writes, and metadata changes (chmod, chown)
- anything run outside the hooked shell

Coreutils are dynamic glibc binaries, so the everyday footguns are
covered. This is a safety net, not a transaction log; keep backups.

## Development

```
make        # build bin/undo and build/libundo.so
make test   # end-to-end tests
```
