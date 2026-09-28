"""Construye un catálogo de imágenes locales y GitHub por contenido Git blob."""

import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
from typing import BinaryIO, Callable
from urllib.parse import quote, urlparse
import zipfile


IMAGE_EXTENSIONS = {".jpg", ".jpeg", ".png", ".webp", ".avif"}
TAR_EXTENSIONS = (".tar", ".tar.gz", ".tgz", ".tar.xz", ".txz", ".tar.bz2", ".tbz", ".tbz2")
HEX_SHA = re.compile(r"[0-9a-f]{40}\Z")
OWNER_REPO = re.compile(r"[A-Za-z0-9_.-]+\Z")


def image_suffix(name: str) -> str | None:
    suffix = PurePosixPath(name).suffix.lower()
    return suffix if suffix in IMAGE_EXTENSIONS else None


def safe_member(name: str) -> bool:
    member = PurePosixPath(name)
    return bool(name) and not member.is_absolute() and ".." not in member.parts and "\\" not in name


def git_blob_id(data: bytes) -> str:
    # Criterio: igualar el SHA de blob que entrega la API Git Trees.
    return hashlib.sha1(f"blob {len(data)}\0".encode() + data).hexdigest()


def _hash_stream(source: BinaryIO, size: int) -> str:
    digest = hashlib.sha1(f"blob {size}\0".encode())
    count = 0
    while chunk := source.read(1024 * 1024):
        count += len(chunk)
        digest.update(chunk)
    if count != size:
        raise ValueError(f"tamaño de imagen cambió: esperado {size}, obtenido {count}")
    return digest.hexdigest()


def _archive_kind(path: Path) -> str | None:
    name = path.name.lower()
    if name.endswith(TAR_EXTENSIONS):
        return "tar"
    if name.endswith(".zip"):
        return "zip"
    if name.endswith(".7z"):
        return "7z"
    return None


def _seven_zip_entries(path: Path) -> list[dict]:
    # Objetivo: calcular hashes en flujo sin extraer el repositorio completo al disco.
    listing = subprocess.run(["7z", "l", "-slt", "-ba", str(path)], check=True,
                             capture_output=True, text=True)
    result = []
    for block in listing.stdout.split("\n\n"):
        fields = dict(line.split(" = ", 1) for line in block.splitlines() if " = " in line)
        name = fields.get("Path", "")
        if (not image_suffix(name) or not safe_member(name) or name.endswith("/")
                or fields.get("Attributes", "").startswith("D") or "Size" not in fields):
            continue
        size = int(fields["Size"])
        with subprocess.Popen(["7z", "x", "-so", "-spd", str(path), name],
                              stdout=subprocess.PIPE, stderr=subprocess.DEVNULL) as process:
            assert process.stdout is not None
            digest = _hash_stream(process.stdout, size)
            if process.wait() != 0:
                raise ValueError(f"no se pudo leer {name} desde {path}")
        result.append({"id": digest, "size": size,
                       "kind": "7z", "path": str(path), "member": name})
    return result


def local_entries(wallpaper_dir: Path) -> list[dict]:
    # Objetivo: indexar originales locales sin modificar archivos sueltos ni comprimidos.
    entries = []
    for root, dirs, files in os.walk(wallpaper_dir):
        if Path(root) == wallpaper_dir:
            dirs[:] = [name for name in dirs if name not in {"index", ".next", "repositories"}]
        for filename in files:
            path = Path(root) / filename
            if path.is_symlink() or not path.is_file():
                continue
            if image_suffix(filename):
                size = path.stat().st_size
                with path.open("rb") as source:
                    digest = _hash_stream(source, size)
                entries.append({"id": digest, "size": size, "kind": "file", "path": str(path)})
                continue
            kind = _archive_kind(path)
            if kind == "7z":
                entries.extend(_seven_zip_entries(path))
            elif kind == "zip":
                with zipfile.ZipFile(path) as archive:
                    for member in archive.infolist():
                        if (not image_suffix(member.filename) or not safe_member(member.filename)
                                or member.is_dir() or stat.S_ISLNK(member.external_attr >> 16)):
                            continue
                        with archive.open(member) as source:
                            digest = _hash_stream(source, member.file_size)
                        entries.append({"id": digest, "size": member.file_size,
                                        "kind": "zip", "path": str(path), "member": member.filename})
            elif kind == "tar":
                with tarfile.open(path, "r:*") as archive:
                    for member in archive.getmembers():
                        if not member.isfile() or not image_suffix(member.name) or not safe_member(member.name):
                            continue
                        source = archive.extractfile(member)
                        if source is None:
                            raise ValueError(f"no se pudo leer {member.name} desde {path}")
                        with source:
                            digest = _hash_stream(source, member.size)
                        entries.append({"id": digest, "size": member.size,
                                        "kind": "tar", "path": str(path), "member": member.name})
    return entries


def extract_local(source: dict, destination: Path) -> None:
    # Objetivo: copiar solo el origen elegido; nunca modificar el archivo fuente.
    kind = source["kind"]
    path = Path(source["path"])
    if kind == "file":
        shutil.copyfile(path, destination)
        return
    member = source["member"]
    if not safe_member(member) or not image_suffix(member):
        raise ValueError("miembro local inválido")
    if kind == "7z":
        with destination.open("wb") as output:
            subprocess.run(["7z", "x", "-so", "-spd", str(path), member],
                           stdout=output, stderr=subprocess.PIPE, check=True)
    elif kind == "zip":
        with zipfile.ZipFile(path) as archive, archive.open(member) as data, destination.open("wb") as output:
            shutil.copyfileobj(data, output)
    elif kind == "tar":
        with tarfile.open(path, "r:*") as archive:
            info = archive.getmember(member)
            if not info.isfile():
                raise ValueError("miembro TAR no es archivo regular")
            data = archive.extractfile(info)
            if data is None:
                raise ValueError("miembro TAR ilegible")
            with data, destination.open("wb") as output:
                shutil.copyfileobj(data, output)
    else:
        raise ValueError(f"origen local desconocido: {kind}")


def parse_repository_url(url: str) -> tuple[str, str]:
    # Criterio: no aceptar hosts, credenciales ni rutas arbitrarias desde el TOML.
    parsed = urlparse(url)
    parts = parsed.path.strip("/").split("/")
    if (parsed.scheme != "https" or parsed.netloc != "github.com" or parsed.username
            or parsed.password or parsed.query or parsed.fragment or len(parts) != 2):
        raise ValueError(f"repositorio GitHub inválido: {url}")
    owner, repo = parts
    repo = repo.removesuffix(".git")
    if not OWNER_REPO.fullmatch(owner) or not OWNER_REPO.fullmatch(repo):
        raise ValueError(f"repositorio GitHub inválido: {url}")
    return owner, repo


def repository_tree(url: str, fetch: Callable[[str], dict]) -> tuple[str, str, list[dict]]:
    # Criterio: fijar contenido al commit y rechazar árboles incompletos.
    owner, repo = parse_repository_url(url)
    base = f"https://api.github.com/repos/{owner}/{repo}"
    info = fetch(base)
    branch = info["default_branch"]
    commit = fetch(f"{base}/commits/{quote(branch, safe='')}")
    sha = commit["sha"]
    tree_sha = commit["commit"]["tree"]["sha"]
    if not HEX_SHA.fullmatch(sha) or not HEX_SHA.fullmatch(tree_sha):
        raise ValueError(f"GitHub devolvió un SHA inválido para {owner}/{repo}")
    tree = fetch(f"{base}/git/trees/{tree_sha}?recursive=1")
    if tree.get("truncated") is not False or not isinstance(tree.get("tree"), list):
        raise ValueError(f"árbol GitHub incompleto para {owner}/{repo}")
    return f"{owner}/{repo}", sha, tree["tree"]


def remote_entries(urls: list[str], fetch: Callable[[str], dict]) -> list[dict]:
    # Objetivo: guardar solo URLs de imágenes públicas fijadas a un commit.
    result = []
    for url in urls:
        repository, commit, tree = repository_tree(url, fetch)
        for item in tree:
            path = item.get("path", "")
            if item.get("type") != "blob" or not image_suffix(path) or not safe_member(path):
                continue
            digest = item.get("sha", "")
            size = item.get("size")
            if not HEX_SHA.fullmatch(digest) or type(size) is not int or size < 0:
                raise ValueError(f"blob remoto inválido: {repository}/{path}")
            result.append({"id": digest, "size": size,
                           "url": f"https://raw.githubusercontent.com/{repository}/{commit}/{quote(path, safe='/')}",
                           "repository": repository, "path": path})
    return result


def merge_records(local: list[dict], remote: list[dict]) -> list[dict]:
    # Criterio C-DEDUP: un contenido aparece una vez aunque tenga varias procedencias.
    merged: dict[str, dict] = {}
    for item in local + remote:
        digest = item["id"]
        size = item["size"]
        record = merged.setdefault(digest, {"id": digest, "size": size, "local": [], "remote": []})
        if record["size"] != size:
            raise ValueError(f"hash con tamaños distintos: {digest}")
        kind = "local" if "kind" in item else "remote"
        source = {key: value for key, value in item.items() if key not in {"id", "size"}}
        if source not in record[kind]:
            record[kind].append(source)
    return [merged[key] for key in sorted(merged)]


def write_jsonl(path: Path, records: list[dict]) -> None:
    # Criterio: un fallo de actualización conserva íntegro el índice anterior.
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_path = tempfile.mkstemp(prefix=".index-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            for record in records:
                output.write(json.dumps(record, ensure_ascii=False) + "\n")
        os.replace(temp_path, path)
    finally:
        Path(temp_path).unlink(missing_ok=True)


def read_jsonl(path: Path) -> list[dict]:
    # Criterio: cron rechaza líneas parciales o referencias sin identidad estable.
    records = []
    seen = set()
    with path.open(encoding="utf-8") as source:
        for number, line in enumerate(source, 1):
            try:
                record = json.loads(line)
                digest = record["id"]
                if (not isinstance(digest, str) or not HEX_SHA.fullmatch(digest)
                        or digest in seen or type(record["size"]) is not int
                        or not isinstance(record["local"], list)
                        or not isinstance(record["remote"], list)
                        or not record["local"] and not record["remote"]):
                    raise ValueError("registro inválido o repetido")
            except (ValueError, TypeError, KeyError) as error:
                raise ValueError(f"index.jsonl línea {number}: {error}") from error
            seen.add(digest)
            records.append(record)
    if not records:
        raise ValueError("index.jsonl está vacío")
    return records
