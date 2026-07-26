{
  description = "undo - revert what the last shell command did to the filesystem";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      packages = forAll (pkgs: rec {
        undo = pkgs.stdenv.mkDerivation {
          pname = "undo";
          version = "0.2.3";
          src = self;
          nativeBuildInputs = [ pkgs.go ];
          # no Go module downloads needed: the CLI is stdlib-only
          buildPhase = ''
            export HOME=$TMPDIR GOCACHE=$TMPDIR/gocache
            make CC=$CC VERSION=v0.2.3
          '';
          installPhase = ''
            make install PREFIX=$out
          '';
          meta = with pkgs.lib; {
            description = "Revert what the last shell command did to the filesystem";
            homepage = "https://github.com/edaywalid/undo";
            license = licenses.mit;
            platforms = platforms.linux;
            mainProgram = "undo";
          };
        };
        default = undo;
      });
    };
}
