{
  description = "codefly clickhouse service: nix runtime (Docker-free)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      # devShell exposes the `clickhouse` multicall binary (clickhouse server,
      # clickhouse client, ...) so the codefly NixEnvironment runs it via
      # `nix develop --command` — no system install required. Mirrors the Docker
      # image (clickhouse/clickhouse-server) the container runtime uses.
      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.clickhouse
            ];
          };
        });
    };
}
