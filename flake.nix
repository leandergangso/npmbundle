{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      lockFile = builtins.fromJSON (builtins.readFile ./package-lock.json);
      packages = if lockFile ? packages then builtins.attrValues lockFile.packages else [ ];
      validDeps = builtins.filter (p: p ? resolved && p ? integrity && p.resolved != "") packages;

      tarballs = map (
        p:
        pkgs.fetchurl {
          url = p.resolved;
          hash = p.integrity;
        }
      ) validDeps;
    in
    {
      packages.${system}.default = pkgs.runCommand "npm-registry-seed" { } ''
        mkdir -p $out/tarballs
        for src in ${toString tarballs}; do
          cp "$src" "$out/tarballs/"
        done
      '';
    };
}

  {
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
        packages.${system}.default = pkgs.runCommand "collect-offline-tarballs" { } ''
          mkdir -p $out/tarballs

          ${pkgs.lib.concatStringsSep "\n" (
            map (drv: ''
              cp ${drv} $out/tarballs/$(basename ${drv})
            '') tarballDrvMap
          )}
        '';
      };
  }
