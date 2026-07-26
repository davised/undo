# Changelog

## v0.2.2 - 2026-07-26

- New `undo upgrade` (and `undo upgrade --check`). It updates a copy
  installed by the one-liner or `make install` in place, and refuses to
  touch a package-managed copy, printing that package manager's command
  instead.
- The shell hooks no longer preload a second copy of the shim when one is
  already present at a different path. Two loaded copies both intercepted
  every call, which duplicated journal entries and recorded each other's
  backup writes into your sessions.
- `undo` chained onto the command it should revert (`rm x && undo`) now
  explains that it needs its own line, instead of printing a pid. The
  documented smoke test was itself written in the broken chained form.
- The installer replaces the binary and shim by atomic rename, so it works
  while they are running or mapped.
- `undo doctor` no longer warns about the parent directory's permissions,
  which is world-readable by design; it checks the session store itself.

## v0.2.1 - 2026-07-26

Fixes a release that did not work on most distributions. Upgrading is
strongly recommended.

- The v0.2.0 shim required glibc 2.38 (a C23 `strtoul` symbol pulled in
  by the build host), so it failed to load on Debian 12, Ubuntu 22.04,
  RHEL 9 and anything older, printing a loader error on every command.
  Release shims are now built against glibc 2.31 and work from 2.6 up.
- `undo run` no longer preloads a second copy of the shim when the shell
  hook is already active, which caused duplicate journal entries.
- CI now asserts the shim's glibc floor and installs the published
  release on Debian, Ubuntu and Fedora on every run.

## v0.2.0 - 2026-07-25

- Ignore rules: the shim skips `node_modules`, `.cache`, `__pycache__`,
  and `.git` by default, plus patterns from `~/.config/undo/ignore` or
  `UNDO_IGNORE`. Keeps build noise out of `undo list` and the store.
- Dedup: repeated in-place writes to the same file within one command
  now keep a single pre-command backup instead of one per write.
- `undo doctor`: checks the shim, libc, store, ignore config, and hooks,
  then runs a live capture/restore self-test on a canary file.
- More packaging channels: curl installer, AUR PKGBUILDs, nix flake.
- Unit tests for the restore engine; e2e coverage for ignore, dedup,
  and doctor.

## v0.1.0 - 2026-07-23

Initial release.

- LD_PRELOAD shim journaling unlink, rename, write opens, rmdir, mkdir,
  symlink, link, truncate, and chmod, with hardlink-or-copy backups.
- Per-command sessions via zsh, bash, and fish hooks, or `undo run` for
  hookless use.
- Undo and redo: the journal replays in both directions and nothing is
  ever permanently deleted by undo itself.
- `undo -i` interactive picker with per-entry cherry-picking.
- `undo diff` content diffs against the pre-command backups.
- Session store is private (0700), size- and count-pruned (`undo gc`),
  and fully removable (`undo purge`).
- Refuses to undo a session whose command may still be running.
