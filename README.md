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
Changed your mind again? `undo redo` re-applies. Nothing undo removes is
ever deleted, only parked in the session store, so undo and redo can
toggle a session back and forth safely.

## How it works

There is no snapshotting and no daemon. A shell hook arms a small
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
```

Then add the hook for your shell:

```
zsh:   echo 'source ~/.local/share/undo/undo.zsh'  >> ~/.zshrc
bash:  echo 'source ~/.local/share/undo/undo.bash' >> ~/.bashrc   # bash >= 5
fish:  echo 'source ~/.local/share/undo/undo.fish' >> ~/.config/fish/config.fish
```

No hook, or a script/CI context? Arm a single command instead:

```
undo run -- ./sketchy-cleanup.sh
```

Requires: Linux, gcc, go. The fish hook is currently untested.

## Usage

```
undo              revert the most recent command that changed files
undo -i           pick a session, cherry-pick entries to restore
undo redo [id]    re-apply an undone session
undo diff [id]    show what a session changed, with content diffs
undo run -- cmd   run one command with the shim armed, no hook needed
undo list         recent sessions
undo show [id]    what a session changed
undo apply <id>   revert a specific session
```

Flags: `-n` dry run, `-y` skip confirmation, `--force` overwrite files
that changed again after the session.

`undo` refuses to clobber a path that was recreated after the session
unless you pass `--force`. A session toggles between applied and undone;
`undo list` marks undone sessions with `u`.

## Configuration

Environment variables, set before sourcing the hook:

- `UNDO_KEEP` - sessions to keep (default 30)
- `UNDO_DATA_DIR` - session store (default `~/.local/share/undo`)
- `UNDO_MAX_BYTES` - largest file the shim will copy as a backup
  (default 256 MiB; only matters for in-place writes, deletions are
  hardlinked and have no size limit)
- `UNDO_CAPTURE_SHELL=1` (zsh only) - re-exec zsh once at startup with
  the shim preloaded, so redirections performed by the shell itself
  (`echo x > file`) are captured too. Without it only child processes
  are covered, which handles `rm`, `mv`, `cp`, `tee`, editors, etc.

## What it cannot catch

`LD_PRELOAD` only reaches dynamically linked programs that go through
libc:

- static binaries and programs that make raw syscalls (Go programs)
- setuid programs; `sudo` strips `LD_PRELOAD` entirely
- writes through already-open file descriptors (`ftruncate`), mmap
  writes, and metadata changes (chmod, chown)
- anything run outside the hooked shell (use `undo run` there)

Coreutils are dynamic glibc binaries, so the everyday footguns are
covered. This is a safety net, not a transaction log; keep backups.

## Development

```
make        # build bin/undo and build/libundo.so
make test   # end-to-end tests
```
