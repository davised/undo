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

Pick your channel:

| Channel | Command |
| --- | --- |
| Homebrew (Linux) | `brew install edaywalid/tap/undo` |
| Debian / Ubuntu | `.deb` from [releases](https://github.com/edaywalid/undo/releases), `sudo dpkg -i undo_*.deb` |
| Fedora / openSUSE | `.rpm` from [releases](https://github.com/edaywalid/undo/releases), `sudo rpm -i undo_*.rpm` |
| Arch | `.pkg.tar.zst` from [releases](https://github.com/edaywalid/undo/releases), `sudo pacman -U` (AUR: `undo-cli`, pending) |
| Nix | `nix run github:edaywalid/undo` (flake, experimental) |
| Any distro, no root | `curl -fsSL https://raw.githubusercontent.com/edaywalid/undo/main/install.sh \| sh` |
| From source | `make install` (needs gcc + go, installs to ~/.local) |

Then add the hook for your shell:

```
zsh:   echo 'source ~/.local/share/undo/undo.zsh'  >> ~/.zshrc
bash:  echo 'source ~/.local/share/undo/undo.bash' >> ~/.bashrc   # bash >= 5
fish:  echo 'source ~/.local/share/undo/undo.fish' >> ~/.config/fish/config.fish
```

Package installs put the hooks in `/usr/share/undo/` instead of
`~/.local/share/undo/`.

No hook, or a script/CI context? Arm a single command instead:

```
undo run -- ./sketchy-cleanup.sh
```

## Platform support

- **Linux, amd64 and arm64.** Any glibc distro: Debian, Ubuntu, Fedora,
  Arch, openSUSE, and friends. WSL2 counts, it is Linux.
- **Alpine / musl**: build from source (`make install`, CI-tested). The
  prebuilt shim in releases targets glibc.
- **Shells**: zsh, bash 5+, fish 3.4+ for the automatic hook. Any shell
  works with `undo run`.
- **macOS**: not supported. The mechanism is Linux LD_PRELOAD plus
  /proc; on macOS, SIP blocks library injection into system binaries,
  so a port would not be able to cover `rm` and friends.
- **Windows**: not supported natively. Use it inside WSL2.
- **Snap / Flatpak**: never. Sandboxes and an LD_PRELOAD hook are
  architecturally incompatible; use a package or the installer.

The fish hook is currently untested.

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
undo gc           prune old, empty, and oversized sessions
undo purge        delete all stored sessions and backups
undo doctor       check the install and run a live capture/restore test
```

If nothing seems to happen after installing, run `undo doctor`: it
locates the shim, checks the store, and deletes then restores a canary
file end to end, so you get a concrete diagnosis instead of silence.

## Ignoring build noise

A command that rewrites thousands of files (a package install, a build)
would otherwise fill `undo list` and the backup store with churn you will
never want to revert. The shim always skips `node_modules`, `.cache`,
`__pycache__`, and `.git`. Add your own patterns in
`~/.config/undo/ignore` (see `examples/ignore`):

```
target        # any directory component named target, at any depth
dist
/home/you/scratch   # an absolute path and everything under it
```

Set `UNDO_DEFAULT_IGNORE=0` to turn off the built-ins, or `UNDO_IGNORE`
to a colon-separated list to override the config file.

Flags: `-n` dry run, `-y` skip confirmation, `--force` overwrite files
that changed again after the session.

`undo` refuses to clobber a path that was recreated after the session
unless you pass `--force`, and refuses to touch a session whose command
may still be running in another terminal. A session toggles between
applied and undone; `undo list` marks undone sessions with `u`.

The session store is kept private (mode 0700) since backups can contain
copies of sensitive files. `undo purge` wipes it entirely.

## Configuration

Environment variables, set before sourcing the hook:

- `UNDO_KEEP` - sessions to keep (default 30)
- `UNDO_MAX_STORE` - total store size budget in bytes (default 1 GiB);
  oldest sessions are pruned first
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
