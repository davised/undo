# Packaging

What ships where, and what each channel needs.

## Already automated (goreleaser, on every tag)

- GitHub release archives (tar.gz, amd64 + arm64, shim included)
- deb, rpm, pkg.tar.zst packages attached to the release
- Homebrew formula pushed to edaywalid/homebrew-tap

## AUR (this directory)

Two PKGBUILDs, both installing the binary as `undo`:

- `undo-cli-bin`: repackages the release artifacts
- `undo-cli`: builds from the release tarball

The AUR names `undo` / `undo-bin` are occupied by an unrelated project
(github.com/nvrmnd-png/undo) whose package provides "undo", hence the
`undo-cli` base name.

To publish (needs an AUR account and the repo to be public):

```
# fill sha256sums from the release checksums.txt first
git clone ssh://aur@aur.archlinux.org/undo-cli-bin.git
cp packaging/aur/undo-cli-bin/PKGBUILD undo-cli-bin/
cd undo-cli-bin && makepkg --printsrcinfo > .SRCINFO
git add PKGBUILD .SRCINFO && git commit -m "0.1.0" && git push
```

Same flow for `undo-cli`.

## Nix

`flake.nix` at the repo root; `nix run github:edaywalid/undo` once the
repo is public. Untested until nix is available locally; treat as
experimental. Upstreaming to nixpkgs is a follow-up once the tool has
users.

## curl installer

`install.sh` at the repo root: detects arch, downloads the latest
release, installs to ~/.local without root, prints hook instructions.
Serve it from the site (or raw.githubusercontent.com) once public.

## Channel requirements and blockers

| Channel            | Works today | Needs                                   |
| ------------------ | ----------- | --------------------------------------- |
| brew tap           | yes         | token while the repo is private         |
| release deb/rpm    | yes         | manual download while private           |
| release pkg.tar.zst| yes         | manual download while private           |
| curl installer     | no          | public releases                         |
| AUR                | no          | public repo + AUR account               |
| nix flake          | no          | public repo                             |
| COPR / OBS repos   | later       | public repo, per-distro maintenance     |
| Debian/Fedora main | much later  | sponsor, review process, project maturity |
| Snap / Flatpak     | never       | sandboxing is incompatible with an LD_PRELOAD hook |
