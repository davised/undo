# undo

**Revert what the last shell command did to the filesystem.**

[![CI](https://github.com/edaywalid/undo/actions/workflows/ci.yml/badge.svg)](https://github.com/edaywalid/undo/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/edaywalid/undo?sort=semver)](https://github.com/edaywalid/undo/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Website: [undo.edaywalid.com](https://undo.edaywalid.com)

```console
$ rm -rf thesis/
$ undo
$ rm -rf thesis/  (14:02:11, 4 changes)
  deleted   thesis/draft.md
  deleted   thesis/refs.bib
  deleted   thesis/notes/
  deleted   thesis/

revert this? [y/N] y
restored 4 change(s)
```

Works for the classic mistakes: `rm` / `rm -rf`, `mv` over a file you
needed, truncating with `>`, an accidental `chmod -R`, files and
directories created by a script that ran amok. Changed your mind again?
`undo redo` re-applies. Undo never deletes anything permanently, only
parks it in the session store, so a session toggles between undone and
applied as many times as you like.

## Why

The shell has never had an undo, and the usual advice is that once data
is overwritten it is gone. The tools that do exist ask you to change
your habits first: alias `rm` to a trash command, or set up btrfs/zfs
snapshots before the accident. undo asks for nothing. You keep typing
`rm`, and the safety net is already under you, even when the deletion
happens three processes deep inside a build tool where no alias could
reach.

It is command-granular, not time-granular: it reverts exactly what *that
one command* touched, on any filesystem, without root. That is the
difference from a snapshot, which rolls the whole tree back to a point in
time and takes your good work with it.

## How it works

There is no snapshotting and no daemon. Nothing runs between your
commands.

1. **Hook.** A shell hook (zsh, bash, fish) arms a small `LD_PRELOAD`
   library, `libundo.so`, around every command you run.
2. **Journal.** While armed, the library intercepts destructive libc
   calls (`unlink`, `rename`, `open` with write flags, `rmdir`, `mkdir`,
   `chmod`, ...). Before each one goes through, it saves the affected
   file into a per-command session and appends a line to a journal.
   Deletions are saved by **hardlink**, so `rm -rf` on gigabytes copies
   no data and costs almost nothing.
3. **Replay.** `undo` reads the last session's journal and replays it in
   reverse: relink deleted files, move renames back, swap truncated
   files with their backups, remove accidental creations, recreate
   directories with their original modes.

Each command that changed something gets one session directory. The
whole storage format is plain files you can inspect:

```
~/.local/share/undo/sessions/        (mode 0700)
└── 1784718280691946/
    ├── cmd        rm -rf thesis/          the command line, for `undo list`
    ├── journal    one line per change     replayed in reverse
    ├── pid, done  liveness markers        so undo won't touch a running command
    └── data/
        ├── 48211-1   = thesis/draft.md    (hardlink, no data copied)
        └── 48211-2   = thesis/refs.bib
```

The last 30 sessions are kept, within a 1 GiB budget; both are
configurable, and `undo purge` wipes the store.

## Install

| Channel | Command |
| --- | --- |
| Homebrew (Linux) | `brew install edaywalid/tap/undo` |
| Debian / Ubuntu | `.deb` from [releases](https://github.com/edaywalid/undo/releases), then `sudo dpkg -i undo_*.deb` |
| Fedora / openSUSE | `.rpm` from [releases](https://github.com/edaywalid/undo/releases), then `sudo rpm -i undo_*.rpm` |
| Arch | `.pkg.tar.zst` from [releases](https://github.com/edaywalid/undo/releases), then `sudo pacman -U ...` (AUR: `undo-cli`, pending) |
| Nix | `nix run github:edaywalid/undo` (flake, experimental) |
| Any distro, no root | `curl -fsSL https://undo.edaywalid.com/install.sh \| sh` |
| From source | `make install` (needs gcc + go, installs to `~/.local`) |

Then add the hook for your shell:

```sh
zsh:   echo 'source ~/.local/share/undo/undo.zsh'  >> ~/.zshrc
bash:  echo 'source ~/.local/share/undo/undo.bash' >> ~/.bashrc            # bash >= 5
fish:  echo 'source ~/.local/share/undo/undo.fish' >> ~/.config/fish/config.fish
```

Package installs put the hooks under `/usr/share/undo/` instead of
`~/.local/share/undo/`. Open a new shell, then confirm it works:

```console
$ undo doctor
$ touch x && rm x && undo
```

No hook, or a script / CI context? Arm a single command instead:

```sh
undo run -- ./sketchy-cleanup.sh
```

## Usage

```
undo              revert the most recent command that changed files
undo -i           pick a session, cherry-pick individual entries to restore
undo redo [id]    re-apply an undone session
undo diff [id]    show what a session changed, with content diffs
undo run -- cmd   run one command with the shim armed, no hook needed
undo list         recent sessions, newest first
undo show [id]    what a session changed
undo apply <id>   revert a specific session
undo gc           prune old, empty, and oversized sessions
undo purge        delete all stored sessions and backups
undo doctor       check the install and run a live capture/restore test
```

Flags: `-n` dry run, `-y` skip the confirmation prompt, `--force`
overwrite files that changed again after the session.

undo asks before reverting, refuses to clobber a path that was recreated
after the session unless you pass `--force`, and refuses to touch a
session whose command may still be running in another terminal. `undo
list` marks undone sessions with a leading `u`.

If nothing seems to happen after installing, run `undo doctor`: it
locates the shim, checks the store, and deletes then restores a canary
file end to end, so you get a concrete diagnosis instead of silence.

## Ignoring build noise

A command that rewrites thousands of files (a package install, a build)
would otherwise flood `undo list` and the store with churn you will never
revert. The shim always skips `node_modules`, `.cache`, `__pycache__`,
and `.git`, and collapses repeated writes to the same file within one
command down to a single backup. Add your own patterns in
`~/.config/undo/ignore` (see [`examples/ignore`](examples/ignore)):

```
target              # any path component named target, at any depth
dist
/home/you/scratch   # an absolute path and everything under it
```

Set `UNDO_DEFAULT_IGNORE=0` to turn the built-ins off, or `UNDO_IGNORE`
to a colon-separated list to override the config file.

## Configuration

Environment variables, set before sourcing the hook:

| Variable | Default | Meaning |
| --- | --- | --- |
| `UNDO_KEEP` | `30` | sessions to keep |
| `UNDO_MAX_STORE` | 1 GiB | total store size budget in bytes; oldest pruned first |
| `UNDO_DATA_DIR` | `~/.local/share/undo` | where sessions live |
| `UNDO_MAX_BYTES` | 256 MiB | largest file the shim copies as a backup (in-place writes only; deletions are hardlinked with no size limit) |
| `UNDO_IGNORE` | (from config) | colon-separated ignore patterns |
| `UNDO_CAPTURE_SHELL=1` | off | zsh only: re-exec once at startup with the shim preloaded so the shell's own redirections (`echo x > file`) are captured too |

## What it cannot catch

`LD_PRELOAD` only reaches dynamically linked programs that go through
libc. undo does **not** see:

- static binaries and programs that make raw syscalls (Go programs)
- `sudo` and setuid programs; the loader strips `LD_PRELOAD`
- writes through already-open file descriptors, `ftruncate`, mmap writes
- `chown` and other metadata beyond mode (`chmod` **is** covered)
- anything run outside the hooked shell (use `undo run` there)

Coreutils and your shell are glibc-dynamic, which is exactly where the
everyday accidents happen. **This is a safety net, not a backup.** Keep
backups.

## Platform support

- **Linux, amd64 and arm64.** Any glibc distro (Debian, Ubuntu, Fedora,
  Arch, openSUSE, ...). WSL2 counts, it is Linux.
- **Alpine / musl**: build from source (`make install`, CI-tested); the
  prebuilt shim in releases targets glibc.
- **Shells**: zsh, bash 5+, fish 3.4+ for the automatic hook. Any shell
  works with `undo run`. The fish hook is currently untested.
- **macOS**: not supported. SIP blocks library injection into system
  binaries, so a port could not cover `rm` and friends.
- **Windows**: use it inside WSL2.
- **Snap / Flatpak**: never; sandboxing and an `LD_PRELOAD` hook are
  architecturally incompatible. Use a package or the installer.

## Development

```sh
make        # build bin/undo and build/libundo.so
make test   # go unit tests + end-to-end suite
```

The end-to-end suite (`test/e2e.sh`) arms the shim exactly as the hooks
do and exercises real deletions, overwrites, renames, undo, redo, ignore
rules, and `gc` against a temp directory. See
[CONTRIBUTING.md](CONTRIBUTING.md) for layout and the release process.

## License

MIT. See [LICENSE](LICENSE).
