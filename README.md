# Wallz

Wallz prepara e intercambia lotes de fondos para DMS. La aplicación Go mantiene
un índice local/remoto y un cursor persistente para que los atajos solo apliquen
imágenes ya preparadas.

## Instalar con Nix

El flake publica `packages.x86_64-linux.default` y `packages.x86_64-linux.wallz`.
El paquete proporciona `wallz`, `rotate-wallpaper` y `wallpaper-index`, e incluye
`config.toml` como configuración inicial. `WALLZ_CONFIG` permite indicar otra
ruta TOML.

Para usar el repositorio privado como input de Home Manager:

```nix
inputs.wallz.url = "git+ssh://git@github.com/jad21/wallz.git";

# En la configuración del usuario:
home.packages = [ inputs.wallz.packages.${pkgs.system}.default ];
```

El wrapper existente `bin/rotate-wallpaper.sh` prefiere el binario Go cuando
está en `PATH`; mientras no se instale, conserva el ejecutor Python como
compatibilidad.

## Comandos

```sh
rotate-wallpaper --next
rotate-wallpaper --prepare
rotate-wallpaper --refill
rotate-wallpaper --rebind-gallery
rotate-wallpaper --next-batch [--limit 1..100]
wallpaper-index folders https://github.com/owner/repository
wallpaper-index refresh
```

`WALLPAPER_DIR`, `CACHE_FILE`, `DMS_BIN` y `WALLZ_CONFIG` permiten ajustar las
rutas usadas por el proceso. El índice y el cursor permanecen en
`$WALLPAPER_DIR/.next`; los datos del cursor siguen en versión 2.
