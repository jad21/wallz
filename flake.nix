{
  description = "Wallz: rotación e indexado de fondos de pantalla";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      wallz = pkgs.buildGoModule {
        pname = "wallz";
        version = "0.1.0";
        src = self;
        subPackages = [ "." ];
        vendorHash = null;
        nativeBuildInputs = [ pkgs.makeWrapper ];
        buildInputs = [ pkgs.p7zip ];
        doCheck = true;

        postInstall = ''
          mkdir -p "$out/libexec" "$out/share/wallz"
          mv "$out/bin/wallz" "$out/libexec/wallz"
          cp ${./config.toml} "$out/share/wallz/config.toml"

          makeWrapper "$out/libexec/wallz" "$out/bin/wallz" \
            --prefix PATH : "${pkgs.p7zip}/bin" \
            --set WALLZ_CONFIG "$out/share/wallz/config.toml"
          makeWrapper "$out/libexec/wallz" "$out/bin/rotate-wallpaper" \
            --prefix PATH : "${pkgs.p7zip}/bin" \
            --set WALLZ_CONFIG "$out/share/wallz/config.toml" \
            --add-flags rotate
          makeWrapper "$out/libexec/wallz" "$out/bin/wallpaper-index" \
            --prefix PATH : "${pkgs.p7zip}/bin" \
            --set WALLZ_CONFIG "$out/share/wallz/config.toml" \
            --add-flags index
        '';

        meta = {
          description = "Rotación e indexado de fondos de pantalla";
          platforms = [ system ];
        };
      };
    in {
      packages.${system} = {
        default = wallz;
        inherit wallz;
      };
    };
}
