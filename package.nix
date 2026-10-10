{ lib, buildGoModule, release }:

let
  plugins = [ "github" "herdr" "cmux" "macos-notify" ];
in
buildGoModule {
  pname = "shephrd";
  version = builtins.substring 0 12 release;
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [ ./go.mod ./go.sum ./cmd ./internal ./plugins ];
  };
  vendorHash = "sha256-0gUoD5YhsymczaFaGMCmREqvl1VLrq6FUs2JveAP9Nw=";
  subPackages = [ "cmd/shephrd" ] ++ map (name: "plugins/${name}") plugins;
  ldflags = [ "-s" "-w" "-X shephrd/internal/version.Release=${release}" ];
  doCheck = false;

  postInstall = ''
    share=$out/share/shephrd
    for name in ${lib.concatStringsSep " " plugins}; do
      mkdir -p $share/plugins/$name/bin
      mv $out/bin/$name $share/plugins/$name/bin/shephrd-$name
      cp plugins/$name/plugin.toml $share/plugins/$name/
      if [ -f plugins/$name/skill.md ]; then cp plugins/$name/skill.md $share/plugins/$name/; fi
    done
    mkdir -p $share/pi
    cp plugins/pi/shephrd-inbox.ts $share/pi/
  '';

  meta = {
    description = "Delegate agent work to isolated workers";
    mainProgram = "shephrd";
  };
}
