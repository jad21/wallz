"""Entrada manual para consultar GitHub y actualizar el catálogo JSONL."""

import json
import os
from pathlib import Path
import tempfile
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import Request, urlopen

from .catalog import (local_entries, merge_records, read_jsonl, remote_entries,
                      repository_tree, write_jsonl, parse_repository_url)
from .config import load_config


def _github_token() -> str | None:
    # Objetivo: leer el PAT opcional desde la configuración privada del usuario.
    config_home = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    token_path = config_home / "wallz" / "github.json"
    try:
        content = token_path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return None
    except UnicodeDecodeError as error:
        raise ValueError(f"archivo de credenciales inválido: {token_path}") from error
    try:
        data = json.loads(content)
    except json.JSONDecodeError as error:
        raise ValueError(f"archivo de credenciales inválido: {token_path}") from error
    if not isinstance(data, dict) or not isinstance(data.get("github_token"), str):
        raise ValueError(f"falta github_token en el archivo de credenciales: {token_path}")
    token = data["github_token"].strip()
    if not token:
        raise ValueError(f"github_token está vacío en el archivo de credenciales: {token_path}")
    return token


def fetch_json(url: str) -> dict:
    # Criterio: consultar solo la API HTTPS de GitHub, autenticar con PAT opcional y limitar la respuesta.
    if urlparse(url).scheme != "https" or urlparse(url).netloc != "api.github.com":
        raise ValueError("solo se permite la API HTTPS de GitHub")

    token = _github_token()
    headers = {"Accept": "application/vnd.github+json",
               "User-Agent": "jad21-wallpaper-index/1"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = Request(url, headers=headers)
    try:
        response = urlopen(request, timeout=25)
    except HTTPError as error:
        # Criterio: un token rechazado permite consultar datos públicos sin autenticar.
        if error.code != 401 or not token:
            raise
        error.close()
        headers.pop("Authorization", None)
        response = urlopen(Request(url, headers=headers), timeout=25)
    with response:
        if urlparse(response.geturl()).netloc != "api.github.com":
            raise ValueError("redirección GitHub inesperada")
        content = response.read(10 * 1024 * 1024 + 1)
    if len(content) > 10 * 1024 * 1024:
        raise ValueError("respuesta GitHub demasiado grande")
    data = json.loads(content)
    if not isinstance(data, dict):
        raise ValueError("respuesta GitHub inválida")
    return data


def folders(url: str, fetch=fetch_json) -> list[str]:
    # Objetivo: informar de las carpetas posibles sin clonar ni descargar imágenes.
    _, _, tree = repository_tree(url, fetch)
    return sorted(item["path"] for item in tree if item.get("type") == "tree")


def _load_progress(path: Path) -> set[str]:
    # Criterio: un marcador corrupto detiene refresh antes de consultar o reemplazar datos.
    if not path.exists():
        return set()
    data = json.loads(path.read_text(encoding="utf-8"))
    completed = data.get("completed") if isinstance(data, dict) else None
    if (data.get("version") != 1 if isinstance(data, dict) else True) or not isinstance(completed, list):
        raise ValueError(".refresh-state.json tiene un formato inválido")
    if any(not isinstance(item, str) for item in completed) or len(set(completed)) != len(completed):
        raise ValueError(".refresh-state.json tiene repositorios inválidos")
    for repository in completed:
        parse_repository_url(f"https://github.com/{repository}.git")
    return set(completed)


def _save_progress(path: Path, completed: set[str]) -> None:
    # Criterio: publicar el checkpoint atómicamente después de guardar el índice.
    descriptor, temp_path = tempfile.mkstemp(prefix=".refresh-state-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump({"version": 1, "completed": sorted(completed)}, output)
        os.replace(temp_path, path)
    finally:
        Path(temp_path).unlink(missing_ok=True)


def _indexed_remote(records: list[dict]) -> list[dict]:
    # Objetivo: conservar las procedencias remotas válidas del índice anterior.
    return [{"id": row["id"], "size": row["size"], **source}
            for row in records for source in row["remote"]]


def refresh(wallpaper_dir: Path, fetch=fetch_json, *, config_path: Path | None = None) -> int:
    # Objetivo: indexar solo repos pendientes y guardar cada éxito para reanudar tras un fallo.
    config = load_config(wallpaper_dir, config_path=config_path)
    state_dir = wallpaper_dir / ".next"
    state_dir.mkdir(parents=True, exist_ok=True)
    index_path = state_dir / "index.jsonl"
    progress_path = state_dir / ".refresh-state.json"
    persisted_records = read_jsonl(index_path) if index_path.exists() else []
    completed = _load_progress(progress_path)
    remote = _indexed_remote(persisted_records)
    for source in remote:
        repository = source.get("repository")
        if isinstance(repository, str):
            completed.add(repository)
    local = local_entries(wallpaper_dir)
    records = merge_records(local, remote)
    failures = []
    for url in config.repositories:
        owner, repo = parse_repository_url(url)
        repository = f"{owner}/{repo}"
        if repository in completed:
            continue
        try:
            added = remote_entries([url], fetch)
        except Exception as error:
            failures.append(f"{repository}: {error}")
            continue
        remote.extend(added)
        updated = merge_records(local, remote)
        if updated:
            write_jsonl(index_path, updated)
            persisted_records = updated
        completed.add(repository)
        _save_progress(progress_path, completed)
        records = updated
    if failures:
        raise ValueError("falló el refresh de: " + "; ".join(failures))
    if not records:
        raise ValueError("no hay imágenes para indexar")
    if records != persisted_records:
        write_jsonl(index_path, records)
    return len(records)
