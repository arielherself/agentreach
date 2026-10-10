{
  description = "reach — point a coding agent at any box you can SSH into";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # The same version, commit and date the Makefile stamps in, so `reach
      # version` reads the same however it was built.
      version = builtins.head (builtins.match ''.*Version = "([^"]+)".*''
        (builtins.readFile ./internal/reach/types.go));
      commit = self.shortRev or self.dirtyShortRev or "unknown";
      date =
        let d = self.lastModifiedDate or "";
        in if d == "" then "unknown"
           else "${builtins.substring 0 4 d}-${builtins.substring 4 2 d}-${builtins.substring 6 2 d}";
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.buildGoModule {
          pname = "reach";
          inherit version;
          src = self;

          # go.mod has no requirements.
          vendorHash = null;

          subPackages = [ "cmd/reach" "cmd/reach-helper" ];
          ldflags = [
            "-s" "-w"
            "-X main.buildVersion=${version}"
            "-X main.buildCommit=${commit}"
            "-X main.buildDate=${date}"
            "-X main.version=${version}"
          ];

          # reach-shell-prefix is the name Claude Code's shell seam has to call
          # reach by. reach keeps its own alias in ~/.reach/bin, but that one
          # points into the store path of whichever build last ran `reach
          # claude`, so it dangles once an upgrade is garbage-collected. A
          # project that wires CLAUDE_CODE_SHELL_PREFIX itself (Claude Desktop)
          # should name ~/.nix-profile/bin/reach-shell-prefix, which every
          # upgrade keeps pointing at the current build.
          postInstall = ''
            ln -s reach $out/bin/reach-shell-prefix
          '';

          nativeCheckInputs = [ pkgs.openssh ];
          # This test runs a launcher written the way version managers write
          # them, `#!/usr/bin/env bash`, and the build sandbox has no
          # /usr/bin/env. `make check` still runs it.
          checkFlags = [ "-skip=^TestShimPassthroughLeavesTheSeamArmed$" ];

          meta = {
            description = "Point your coding agent at any box you can SSH into";
            homepage = "https://github.com/bojieli/agentreach";
            license = pkgs.lib.licenses.mit;
            mainProgram = "reach";
          };
        };
      });
    };
}
