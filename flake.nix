{
  description = "npm deps generator";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      # 1. Read package-lock.json as JSON
      lockFile = builtins.fromJSON (builtins.readFile ./package-lock.json);

      # 2. Extract packages (handling npm v2/v3 'packages' map or legacy 'dependencies')
      packagesMap = if lockFile ? packages then lockFile.packages else lockFile.dependencies;

      # 3. Filter out the root project entry ("") and anything missing a resolved URL
      deps = pkgs.lib.filterAttrs (
        path: details: path != "" && details ? resolved && details.resolved != null
      ) packagesMap;

      # 4. Turn every dependency into a fetchurl derivation
      tarballDrvMap = pkgs.lib.mapAttrsToList (
        path: details:
        pkgs.fetchurl {
          url = details.resolved;
          hash = details.integrity;
        }
      ) deps;

    in
    {
      packages.${system}.default = pkgs.runCommand "collect-tarballs" { } ''
        mkdir -p $out/tarballs

        ${pkgs.lib.concatStringsSep "\n" (
          map (drv: ''
            cp -f ${drv} $out/tarballs/$(basename ${drv})
          '') tarballDrvMap
        )}
      '';
    };
}
