{
  description = "phial: a memory server for coding agents";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      version = "0.1.0";
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule {
            pname = "phial";
            inherit version;
            src = ./.;
            vendorHash = "sha256-7KNOhuHTDkZv99iITkUm4WImkC4R/4xkZ22W8kfdpBU=";
            subPackages = [
              "cmd/phial"
              "cmd/phial-client"
            ];
            ldflags = [
              "-s"
              "-w"
              "-X github.com/jaspermayone/phial/internal/mcpserver.Version=${version}"
              "-X main.version=${version}"
            ];
            meta = {
              description = "A memory server for coding agents";
              homepage = "https://github.com/jaspermayone/phial";
              license = pkgs.lib.licenses.mit;
              mainProgram = "phial";
            };
          };
        }
      );
    };
}
