# Changelog

## Unreleased

- Ignore rules: the shim skips `node_modules`, `.cache`, `__pycache__`,
  and `.git` by default, plus patterns from `~/.config/undo/ignore` or
  `UNDO_IGNORE`. Keeps build noise out of `undo list` and the store.
- `undo doctor`: checks the shim, libc, store, ignore config, and hooks,
  then runs a live capture/restore self-test on a canary file.

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
