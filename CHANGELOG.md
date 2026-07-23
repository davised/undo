# Changelog

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
