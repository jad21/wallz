#!/usr/bin/env python3
"""Entradas separadas: rotación local, reposición de faltantes y salto manual."""

import argparse
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.engine import WallpaperEngine
from wallpaper_rotation.config import load_config


def main() -> int:
    # Objetivo: elegir una sola operación y permitir un tamaño puntual al reemplazar el lote.
    parser = argparse.ArgumentParser(description="Rotación y preparación de lotes de fondos")
    actions = parser.add_mutually_exclusive_group(required=True)
    actions.add_argument("--next", dest="action", action="store_const", const="next")
    actions.add_argument("--prepare", dest="action", action="store_const", const="prepare")
    actions.add_argument("--refill", dest="action", action="store_const", const="refill")
    actions.add_argument("--rebind-gallery", dest="action", action="store_const",
                         const="rebind-gallery")
    actions.add_argument("--next-batch", dest="action", action="store_const",
                         const="next-batch")
    parser.add_argument("--limit", type=int, help="cantidad de imágenes para --next-batch (1-100)")
    args = parser.parse_args()
    if args.limit is not None:
        if args.action != "next-batch":
            parser.error("--limit solo se puede usar con --next-batch")
        if not 1 <= args.limit <= 100:
            parser.error("--limit debe estar entre 1 y 100")

    wallpaper_dir = Path(os.environ.get("WALLPAPER_DIR", "/home/jad21/Imágenes/wallpaper"))
    cache_file = Path(os.environ.get("CACHE_FILE", "/home/jad21/.cache/current-wallpaper"))
    dms_bin = Path(os.environ.get("DMS_BIN", "/home/jad21/.nix-profile/bin/dms"))
    config_path = Path(os.environ.get("WALLZ_CONFIG", "/home/jad21/develop/wallz/config.toml"))
    config = load_config(wallpaper_dir, config_path=config_path)
    batch_size = args.limit if args.limit is not None else config.batch_size
    engine = WallpaperEngine(wallpaper_dir, cache_file, dms_bin, batch_size=batch_size)
    if args.action == "prepare":
        engine.prepare()
    elif args.action == "refill":
        engine.refill()
    elif args.action == "rebind-gallery":
        engine.rebind_current()
    elif args.action == "next-batch":
        engine.skip_and_prepare()
        engine.rotate()
    else:
        try:
            remaining = engine.rotate()
        except ValueError as error:
            if "lote agotado" not in str(error) and "lote no preparado" not in str(error):
                raise
            _request_refill()
            raise
        if remaining == 0:
            # Criterio: solicitar reposición sin esperar descargas en el proceso del atajo.
            _request_refill()
    return 0


def _request_refill() -> None:
    # Objetivo: activar el repositor independiente sin bloquear el cambio visible.
    queued = subprocess.run(["systemctl", "--user", "start", "--no-block",
                             "refill-wallpaper.service"], check=False)
    if queued.returncode:
        print("rotate-wallpaper: no se pudo solicitar la reposición", file=sys.stderr)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, IndexError, subprocess.CalledProcessError) as error:
        print(f"rotate-wallpaper: {error}", file=sys.stderr)
        sys.exit(1)
