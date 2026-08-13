{
  description = "Persistent PTY shell with Codex agent dispatch";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule {
            pname = "harnesh";
            version = "0-unstable-2026-08-12";
            src = ./.;

            vendorHash = "sha256-ciGayPKX2j48v7nO6TUUCqynF6Q7vHMdnVgz3ZW8bo8=";

            nativeBuildInputs = [ pkgs.makeWrapper ];

            ldflags = [ "-s" "-w" ];

            postInstall = ''
              wrapProgram $out/bin/harnesh \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.coreutils ]}
            '';

            meta = {
              description = "Persistent PTY shell with Codex agent dispatch";
              homepage = "https://github.com/cameron/harnesh";
              mainProgram = "harnesh";
              platforms = nixpkgs.lib.platforms.linux;
            };
          };
        });

      nixosModules.default = { pkgs, ... }: {
        environment.systemPackages = [
          self.packages.${pkgs.stdenv.hostPlatform.system}.default
        ];
      };
    };
}
