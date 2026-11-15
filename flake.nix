{
  description = "SilkRouter: route any command through fast, scanned and ranked proxies (px / s-proxy / silkrouter)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "4.0.0-dev+" + (self.shortRev or self.dirtyShortRev or "unknown");
      mkPackage =
        pkgs:
        pkgs.callPackage ./packaging/nix/package.nix {
          inherit version;
          src = self;
        };
    in
    {
      packages = forAllSystems (pkgs: rec {
        silkrouter = mkPackage pkgs;
        default = silkrouter;
      });

      apps = forAllSystems (
        pkgs:
        let
          pkg = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        in
        {
          default = {
            type = "app";
            program = "${pkg}/bin/silkrouter";
            meta.description = "SilkRouter CLI";
          };
          px = {
            type = "app";
            program = "${pkg}/bin/px";
            meta.description = "SilkRouter CLI (px alias)";
          };
        }
      );

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "silkrouter-dev";
          packages =
            with pkgs;
            [
              (pkgs.go_1_26 or go)
              gopls
              gotools
              golangci-lint
              delve
              gnumake
              git
              sqlite
              zsh
              fish
              nixfmt-rfc-style
            ]
            ++ lib.optionals pkgs.stdenv.isLinux [ proxychains-ng ];
          CGO_ENABLED = "0";
          shellHook = ''
            echo "silkrouter dev shell: make (help), make check, make build, make tui"
          '';
        };
      });

      overlays.default = final: _prev: { silkrouter = mkPackage final; };

      homeManagerModules.default = import ./packaging/nix/hm-module.nix self;
      homeManagerModules.silkrouter = self.homeManagerModules.default;

      checks = forAllSystems (pkgs: {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
