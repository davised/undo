# Publishing to the AUR

Both PKGBUILDs are checksum-filled and build-tested against the v0.2.0
release. `.SRCINFO` files are generated and current. What is left needs
an AUR account with your SSH key registered (https://aur.archlinux.org).

## One-time, per package

```sh
# binary package (fast install, repackages the release artifacts)
git clone ssh://aur@aur.archlinux.org/undo-cli-bin.git /tmp/aur-bin
cp packaging/aur/undo-cli-bin/{PKGBUILD,.SRCINFO} /tmp/aur-bin/
cd /tmp/aur-bin && git add -A && git commit -m "undo-cli-bin 0.2.0" && git push

# source package (builds from the release tarball)
git clone ssh://aur@aur.archlinux.org/undo-cli.git /tmp/aur-src
cp packaging/aur/undo-cli/{PKGBUILD,.SRCINFO} /tmp/aur-src/
cd /tmp/aur-src && git add -A && git commit -m "undo-cli 0.2.0" && git push
```

An empty clone warning is expected for a package that does not exist
yet; the first push creates it.

## On every release

1. Bump `pkgver` in both PKGBUILDs.
2. Update `sha256sums`: binary sums come from the release `checksums.txt`,
   the source sum from `sha256sum` of the tag tarball. `updpkgsums` does
   both automatically.
3. Regenerate: `makepkg --printsrcinfo > .SRCINFO`.
4. Commit and push each AUR repo.

## Naming note

The AUR names `undo` and `undo-bin` are taken by an unrelated project
that also declares `provides=(undo)`, so these use the `undo-cli` base.
The installed binary is still `undo`.
