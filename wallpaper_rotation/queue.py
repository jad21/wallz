"""Prepara lotes de fondos y conserva el cursor de consumo de forma atómica."""

from dataclasses import dataclass, replace
import json
import os
from pathlib import Path
import random
import re
import shutil
import tempfile
from typing import Callable, Sequence, TypeVar
from uuid import uuid4


T = TypeVar("T")
GENERATION = re.compile(r"batch-[0-9a-f]{32}\Z")
IMAGE_FILE = re.compile(r"[0-9]{4}\.(?:jpg|jpeg|png|webp|avif)\Z")


class QueueError(Exception):
    """Contrato: el lote o su cursor no puede usarse sin intervención."""


@dataclass(frozen=True)
class QueueEntry:
    """Contrato: una imagen preparada y la identidad de su origen."""

    file: str
    source: str


@dataclass(frozen=True)
class QueueState:
    """Contrato: position indica la siguiente entrada sin consumir."""

    generation: str
    position: int
    entries: tuple[QueueEntry, ...]


def load_state(next_dir: Path) -> QueueState | None:
    # Criterio Q-BROKEN: distinguir ausencia inicial de cursor dañado o lote incompleto.
    cursor = next_dir / ".cursor.json"
    if not cursor.exists():
        return None
    try:
        data = json.loads(cursor.read_text(encoding="utf-8"))
        generation = data["generation"]
        position = data["position"]
        raw_entries = data["entries"]
        if (data.get("version") != 1 or not isinstance(generation, str)
                or not GENERATION.fullmatch(generation) or type(position) is not int
                or not isinstance(raw_entries, list) or not raw_entries
                or not 0 <= position <= len(raw_entries)):
            raise ValueError("estructura del cursor inválida")
        folder = next_dir / generation
        if not folder.is_dir() or folder.is_symlink():
            raise ValueError("directorio del lote inválido")
        entries = []
        for item in raw_entries:
            if not isinstance(item, dict) or not isinstance(item.get("file"), str) or not isinstance(item.get("source"), str):
                raise ValueError("entrada del cursor inválida")
            file = item["file"]
            parts = Path(file).parts
            if (len(parts) != 2 or parts[0] != generation or not IMAGE_FILE.fullmatch(parts[1])
                    or not item["source"]):
                raise ValueError("ruta del cursor inválida")
            image = next_dir / file
            if not image.is_file() or image.is_symlink():
                raise ValueError(f"imagen preparada ausente: {file}")
            entries.append(QueueEntry(file, item["source"]))
        if len({entry.source for entry in entries}) != len(entries):
            raise ValueError("orígenes repetidos en el cursor")
        return QueueState(generation, position, tuple(entries))
    except (OSError, ValueError, TypeError, KeyError, AttributeError) as error:
        raise QueueError(f".cursor.json inválido: {error}") from error


def save_state(next_dir: Path, state: QueueState) -> None:
    # Criterio: el cursor anterior persiste completo si falla la escritura del nuevo.
    data = {"version": 1, "generation": state.generation, "position": state.position,
            "entries": [{"file": entry.file, "source": entry.source} for entry in state.entries]}
    descriptor, temp_path = tempfile.mkstemp(prefix=".cursor-", dir=next_dir)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as output:
            json.dump(data, output, ensure_ascii=False)
        os.replace(temp_path, next_dir / ".cursor.json")
    finally:
        Path(temp_path).unlink(missing_ok=True)


def _clean_old_generations(next_dir: Path, current: str) -> None:
    # Criterio: borrar solo lotes generados con el patrón privado del rotador.
    for path in next_dir.iterdir():
        if path.name != current and GENERATION.fullmatch(path.name) and path.is_dir() and not path.is_symlink():
            shutil.rmtree(path)


def ensure_batch(
    next_dir: Path,
    batch_size: int,
    catalog: Callable[[], Sequence[T]],
    identity: Callable[[T], str],
    suffix: Callable[[T], str],
    materialize: Callable[[T, Path], None],
) -> QueueState:
    # Objetivo: preparar un lote completo antes de reemplazar el cursor visible.
    if next_dir.is_symlink():
        raise QueueError(".next no puede ser un enlace simbólico")
    next_dir.mkdir(exist_ok=True)
    previous = load_state(next_dir)
    if previous is not None and previous.position < len(previous.entries) and len(previous.entries) == batch_size:
        return previous
    options = list(catalog())
    unique = {identity(option): option for option in options}
    if len(unique) < batch_size:
        raise QueueError(f"se necesitan {batch_size} imágenes distintas; hay {len(unique)}")
    prior_sources = {entry.source for entry in previous.entries} if previous else set()
    fresh = [option for key, option in unique.items() if key not in prior_sources]
    selected = random.sample(fresh, min(len(fresh), batch_size))
    used = {identity(option) for option in selected}
    if len(selected) < batch_size:
        remainder = [option for key, option in unique.items() if key not in used]
        selected.extend(random.sample(remainder, batch_size - len(selected)))
    random.shuffle(selected)

    generation = f"batch-{uuid4().hex}"
    folder = next_dir / generation
    folder.mkdir()
    committed = False
    try:
        entries = []
        for index, option in enumerate(selected):
            name = f"{index:04}{suffix(option)}"
            materialize(option, folder / name)
            entries.append(QueueEntry(f"{generation}/{name}", identity(option)))
        state = QueueState(generation, 0, tuple(entries))
        save_state(next_dir, state)
        committed = True
    finally:
        if not committed:
            shutil.rmtree(folder)
    _clean_old_generations(next_dir, generation)
    return state


def advance(next_dir: Path, state: QueueState) -> QueueState:
    # Criterio Q-READY: registrar el uso solo tras la aceptación de DMS.
    if state.position >= len(state.entries):
        raise QueueError("lote agotado")
    advanced = replace(state, position=state.position + 1)
    save_state(next_dir, advanced)
    return advanced
