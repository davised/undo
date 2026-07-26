# Packaging

What ships where, and who generates it.

## Automated on every tag

| Artifact | Produced by |
| --- | --- |
| tar.gz archives (amd64, arm64) | goreleaser |
| deb, rpm, pkg.tar.zst | goreleaser (nfpms) |
| Homebrew formula in edaywalid/homebrew-tap | goreleaser (`brews:`) |
| **AUR `undo-cli-bin`** | goreleaser (`aurs:`) |
| **AUR `undo-cli`** (source) | `packaging/aur/render.sh`, run by the release workflow |

Nothing here is hand-edited. That is deliberate: `pkgver` and the
checksums used to be maintained by hand in two files and drifted apart,
shipping a package whose sums did not match its version, which fails for
every user who installs it.

## Secrets the release workflow expects

| Secret | Used for | Without it |
| --- | --- | --- |
| `TAP_GITHUB_TOKEN` | pushing the Homebrew formula | formula step fails, release still publishes |
| `AUR_KEY` | AUR SSH key (unencrypted private key, contents not path) | both AUR steps skip cleanly |

## Why two mechanisms for AUR

goreleaser's `aurs:` writes the binary package and takes its checksums
directly from the archives it just built, so nothing can drift. Arch
guidelines reserve the plain name for packages built from source, which
is why it is `undo-cli-bin`.

goreleaser cannot also produce the source package: configuring `aurs`
and `aur_sources` together is a [known
panic](https://goreleaser.com/customization/aursources/). So `undo-cli`
is rendered by `render.sh` from `PKGBUILD.src.in`, hashing the release
tarball itself, and verified before it is pushed.

The AUR names `undo` and `undo-bin` were already taken by an unrelated
project that declares `provides=(undo)`, hence the `undo-cli` base. The
installed binary is still `undo`.

## Doing it by hand

```sh
packaging/aur/render.sh v0.2.6      # writes packaging/aur/undo-cli/PKGBUILD
cd packaging/aur/undo-cli && makepkg -f   # optional: build it locally first
```

Then copy `PKGBUILD` and `.SRCINFO` into a clone of
`ssh://aur@aur.archlinux.org/undo-cli.git` and push.

The generated `PKGBUILD` and `.SRCINFO` are gitignored. The template and
the script are the sources of truth.

## Channels not covered

- **COPR / OBS**: possible later, needs per-distro maintenance.
- **Debian / Fedora proper**: needs a sponsor and a mature project.
- **Snap / Flatpak**: never. Sandboxing and an `LD_PRELOAD` hook are
  architecturally incompatible.
