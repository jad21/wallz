"""Contrato del TOML del proyecto y validación de repositorios/tamaño de lote."""

from dataclasses import dataclass
import os
from pathlib import Path
import shutil
import tempfile
import tomllib

from .catalog import parse_repository_url


@dataclass(frozen=True)
class RotationConfig:
    """Objetivo: mantener repositorios y tamaño del lote en un solo TOML."""

    repositories: tuple[str, ...]
    batch_size: int


def load_config(wallpaper_dir: Path, *, config_path: Path | None = None) -> RotationConfig:
    # Criterio: rechazar configuración ausente o inválida antes de usar el índice/cursor.
    source_path = config_path or (wallpaper_dir / ".next" / "config.toml")
    with source_path.open("rb") as source:
        data = tomllib.load(source)
    urls = data.get("repositories")
    size = data.get("batch_size")
    if (not isinstance(urls, list) or not urls or any(not isinstance(url, str) for url in urls)
            or len(set(urls)) != len(urls)):
        raise ValueError("config.toml necesita repositorios públicos únicos")
    for url in urls:
        parse_repository_url(url)
    if type(size) is not int or not 1 <= size <= 100:
        raise ValueError("config.toml: batch_size debe estar entre 1 y 100")
    return RotationConfig(tuple(urls), size)


def migrate_legacy(wallpaper_dir: Path) -> None:
    # Objetivo: conservar .next y su cursor; incorporar allí solo la configuración y DB heredadas.
    state = wallpaper_dir / ".next"
    if state.is_symlink():
        raise ValueError(".next no puede ser un enlace simbólico")
    state.mkdir(exist_ok=True)
    old_config = wallpaper_dir / "repositories" / "repos.toml"
    old_index = wallpaper_dir / "index" / "index.jsonl"
    config = state / "config.toml"
    index = state / "index.jsonl"
    if not config.exists():
        with old_config.open("rb") as source:
            data = tomllib.load(source)
        urls = data.get("repositories")
        if not isinstance(urls, list) or not urls:
            raise ValueError("repos.toml anterior inválido")
        # Criterio: los dos archivos nuevos se publican antes de retirar los anteriores.
        descriptor, temp_path = tempfile.mkstemp(prefix=".config-", dir=state)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                output.write("# Objetivo: único punto editable para repositorios y tamaño del lote.\n")
                output.write("repositories = [\n")
                for url in urls:
                    output.write(f"    {url!r},\n")
                output.write("]\nbatch_size = 10\n")
            os.replace(temp_path, config)
        finally:
            Path(temp_path).unlink(missing_ok=True)
    load_config(wallpaper_dir, config_path=config)
    if not index.exists():
        descriptor, temp_path = tempfile.mkstemp(prefix=".index-", dir=state)
        os.close(descriptor)
        try:
            shutil.copyfile(old_index, temp_path)
            os.replace(temp_path, index)
        finally:
            Path(temp_path).unlink(missing_ok=True)
    # Criterio: los originales retirados conservan una copia de recuperación dentro de .next.
    backup = state / ".migration-backup"
    backup.mkdir(exist_ok=True)
    for original in (old_config, old_index):
        if original.exists():
            saved = backup / original.name
            if not saved.exists():
                shutil.copy2(original, saved)
    if old_config.exists():
        old_config.unlink()
    if old_index.exists():
        old_index.unlink()
    # Criterio: archivar metadatos v1 generados sin eliminar archivos ajenos.
    old_index_dir = wallpaper_dir / "index"
    if old_index_dir.is_dir() and not old_index_dir.is_symlink():
        for meta in old_index_dir.glob("*.meta.json"):
            if meta.is_file() and not meta.is_symlink():
                legacy_backup = backup / "legacy-index"
                legacy_backup.mkdir(exist_ok=True)
                shutil.copy2(meta, legacy_backup / meta.name)
                meta.unlink()
        if not any(old_index_dir.iterdir()):
            old_index_dir.rmdir()
    old_repos_dir = wallpaper_dir / "repositories"
    if old_repos_dir.is_dir() and not old_repos_dir.is_symlink() and not any(old_repos_dir.iterdir()):
        old_repos_dir.rmdir()
