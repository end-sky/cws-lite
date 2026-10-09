{
  description = "CWS Lite — lightweight Chrome Web Store front end";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.buildGoModule {
            pname = "cws-lite";
            version = "0.1.0";
            src = ./.;
            vendorHash = null;
            subPackages = [ "." ];
            ldflags = [ "-s" "-w" ];
            meta = {
              description = "Lightweight front end for Chrome Web Store extensions and themes";
              license = pkgs.lib.licenses.mit;
              mainProgram = "cws-lite";
            };
          };
        });

      devShells = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go ];
          };
        });
    };
}
