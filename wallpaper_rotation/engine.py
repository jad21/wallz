"""Orquesta lotes mixtos, descargas verificadas y el cursor visible de DMS."""

from dataclasses import replace
import filecmp
import fcntl
import hashlib
import json
import os
from pathlib import Path
import random
import re
import shutil
import subprocess
import tempfile
from typing import Callable
from urllib.parse import urlparse
from urllib.request import Request, urlopen
from uuid import uuid4

from .catalog import IMAGE_EXTENSIONS, extract_local, read_jsonl
from .cycle import CycleItem, CycleState, ensure_upcoming, initial_state, mark_applied, skip_current


MAX_IMAGE_BYTES = 100 * 1024 * 1024
GENERATION = re.compile(r"batch-[0-9a-f]{32}\Z")


def _image_suffix(source: dict) -> str:
    name = source.get("member") or source.get("path") or source.get("url", "")
    suffix = Path(urlparse(name).path).suffix.lower()
    if suffix not in IMAGE_EXTENSIONS:
        raise ValueError(f"imagen sin extensión compatible: {name}")
    return suffix


def _verify_file(path: Path, record: dict) -> None:
    # Criterio: una descarga o extracción incompleta jamás llega a DMS.
    size = record["size"]
    if size > MAX_IMAGE_BYTES or path.stat().st_size != size:
        raise ValueError("imagen demasiado grande o de tamaño inesperado")
    digest = hashlib.sha1(f"blob {size}\0".encode())
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    if digest.hexdigest() != record["id"]:
        raise ValueError("hash de imagen distinto al index.jsonl")


def download_remote(record: dict, source: dict, target: Path) -> None:
    # Criterio: aceptar únicamente URLs RAW GitHub indexadas y fijadas a un commit.
    url = source["url"]
    parsed = urlparse(url)
    parts = parsed.path.strip("/").split("/")
    if (parsed.scheme != "https" or parsed.netloc != "raw.githubusercontent.com"
            or len(parts) < 4 or not re.fullmatch(r"[0-9a-f]{40}", parts[2])):
        raise ValueError("URL remota fuera de GitHub RAW o sin commit")
    if record["size"] > MAX_IMAGE_BYTES:
        raise ValueError("imagen remota excede 100 MiB")
    request = Request(url, headers={"User-Agent": "jad21-wallpaper-rotate/1"})
    with urlopen(request, timeout=30) as response:
        if urlparse(response.geturl()).netloc != "raw.githubusercontent.com":
            raise ValueError("redirección remota inesperada")
        total = 0
        with target.open("wb") as output:
            while chunk := response.read(1024 * 1024):
                total += len(chunk)
                if total > MAX_IMAGE_BYTES:
                    raise ValueError("descarga remota demasiado grande")
                output.write(chunk)


def _serialize_cycle(cycle: CycleState) -> dict:
    return {"order": [{"id": item.id, "epoch": item.epoch} for item in cycle.order],
            "position": cycle.position, "current_id": cycle.current_id,
            "skipped": list(cycle.skipped), "source_turn": cycle.source_turn}


def _deserialize_cycle(data: dict) -> CycleState:
    # Criterio: rechazar posiciones y estados corruptos antes de cualquier efecto.
    order = tuple(CycleItem(item["id"], item["epoch"]) for item in data["order"])
    position = data["position"]
    if (type(position) is not int or not 0 <= position <= len(order)
            or data["source_turn"] not in {"local", "remote"}):
        raise ValueError(".cursor.json tiene un ciclo inválido")
    return CycleState(order, position, data["current_id"],
                      tuple(data["skipped"]), data["source_turn"])


class WallpaperEngine:
    """Contrato: preparar N, mostrar su galería, avanzar cursor y limpiar solo copias propias."""

    def __init__(self, wallpaper_dir: Path, cache_file: Path, dms_bin: Path, *,
                 batch_size: int = 10, rng: random.Random | None = None,
                 downloader: Callable[[dict, dict, Path], None] = download_remote):
        if not 1 <= batch_size <= 100:
            raise ValueError("el lote debe tener entre 1 y 100 imágenes")
        self.wallpaper_dir = wallpaper_dir
        self.cache_file = cache_file
        self.dms_bin = dms_bin
        self.batch_size = batch_size
        self.rng = rng or random.Random()
        self.downloader = downloader
        self.next_dir = wallpaper_dir / ".next"
        self.managed_dir = cache_file.parent / "rotate-wallpaper"

    def _current_id(self, records: dict[str, dict]) -> str | None:
        # Criterio: contar el fondo ya visible durante la migración desde cursor v1.
        try:
            active = Path(self.cache_file.read_text(encoding="utf-8").strip())
            size = active.stat().st_size
            digest = hashlib.sha1(f"blob {size}\0".encode())
            with active.open("rb") as source:
                while chunk := source.read(1024 * 1024):
                    digest.update(chunk)
            identifier = digest.hexdigest()
            return identifier if identifier in records else None
        except (OSError, ValueError):
            return None

    def _load(self, records: dict[str, dict] | None) -> dict:
        cursor = self.next_dir / ".cursor.json"
        if not cursor.exists():
            if records is None:
                raise ValueError("lote no preparado; espere al servicio de preparación")
            return {"version": 2, "cycle": initial_state(list(records), self._current_id(records), self.rng),
                    "generation": None, "batch": [], "batch_position": 0, "cleanup_remote": None}
        data = json.loads(cursor.read_text(encoding="utf-8"))
        if data.get("version") == 1:
            if records is None:
                raise ValueError("lote v1 requiere preparación antes de usar el atajo")
            backup = self.next_dir / ".cursor.v1.backup.json"
            if not backup.exists():
                shutil.copy2(cursor, backup)
            return {"version": 2, "cycle": initial_state(list(records), self._current_id(records), self.rng),
                    "generation": data.get("generation"), "batch": [], "batch_position": 0,
                    "cleanup_remote": None, "selection_policy": "balanced-v1"}
        if data.get("version") != 2:
            raise ValueError("versión de .cursor.json desconocida")
        cycle = _deserialize_cycle(data["cycle"])
        position = data["batch_position"]
        batch = data["batch"]
        if type(position) is not int or not 0 <= position <= len(batch):
            raise ValueError("posición del lote inválida")
        for entry in batch[position:]:
            file = entry["file"]
            parts = Path(file).parts
            if (len(parts) != 2 or parts[0] not in {data["generation"], "downloads"}
                    or not (self.next_dir / file).is_file()
                    or (self.next_dir / file).is_symlink()):
                raise ValueError("imagen pendiente ausente o fuera de .next")
        data["cycle"] = cycle
        if data.get("selection_policy") != "balanced-v1" and position == 0:
            data["needs_rebalance"] = True
        return data

    def _save(self, state: dict) -> None:
        # Criterio: actualizar cursor completo mediante renombrado atómico.
        serial = dict(state)
        serial["cycle"] = _serialize_cycle(state["cycle"])
        descriptor, temp_path = tempfile.mkstemp(prefix=".cursor-", dir=self.next_dir)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                json.dump(serial, output, ensure_ascii=False)
            os.replace(temp_path, self.next_dir / ".cursor.json")
        finally:
            Path(temp_path).unlink(missing_ok=True)

    def _materialize(self, record: dict, turn: str, generation: str, slot: int) -> dict:
        # Objetivo: elegir una fuente, verificarla y degradar a la otra si falla.
        local = record["local"]
        remote = record["remote"]
        choices = (["remote", "local"] if turn == "remote" else ["local", "remote"])
        last_error = None
        for kind in choices:
            sources = remote if kind == "remote" else local
            for source in sources:
                suffix = _image_suffix(source)
                name = f"{generation}/{slot:04}{suffix}"
                target = self.next_dir / name
                temp = target.with_name(target.name + ".part")
                try:
                    if kind == "remote":
                        self.downloader(record, source, temp)
                    else:
                        extract_local(source, temp)
                    _verify_file(temp, record)
                    os.replace(temp, target)
                    return {"id": record["id"], "file": name, "source": kind}
                except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
                    last_error = error
                    temp.unlink(missing_ok=True)
        raise ValueError(f"ninguna fuente disponible para {record['id']}: {last_error}")

    def _prepare_locked(self, records: dict[str, dict], state: dict) -> dict:
        rebalance = state.get("needs_rebalance", False)
        if state["batch_position"] < len(state["batch"]) and not rebalance:
            return self._unify_gallery(records, state)
        generation = f"batch-{uuid4().hex}"
        folder = self.next_dir / generation
        folder.mkdir()
        (self.next_dir / "downloads").mkdir(exist_ok=True)
        cycle = state["cycle"]
        batch = []
        failed = 0
        turn = cycle.source_turn
        committed = False
        retired = state["batch"] if rebalance else []
        try:
            while len(batch) < self.batch_size:
                if failed > len(records) * 2:
                    raise ValueError("no hay diez fuentes utilizables; se conserva el fondo actual")
                cycle = ensure_upcoming(cycle, list(records), self.batch_size, self.rng)
                index = cycle.position + len(batch)
                # Criterio: priorizar el turno local/remoto sin cruzar un ciclo ni repetir hashes.
                epoch = cycle.order[index].epoch
                preferred = next((candidate for candidate in range(index, len(cycle.order))
                                  if cycle.order[candidate].epoch == epoch
                                  and (record := records.get(cycle.order[candidate].id))
                                  and record[turn]), None)
                if preferred is not None and preferred != index:
                    ordered = list(cycle.order)
                    ordered[index], ordered[preferred] = ordered[preferred], ordered[index]
                    cycle = replace(cycle, order=tuple(ordered))
                item = cycle.order[index]
                record = records.get(item.id)
                if record is None:
                    failed += 1
                    cycle = replace(cycle, order=cycle.order[:index] + cycle.order[index + 1:],
                                    skipped=cycle.skipped + (item.id,))
                    continue
                try:
                    entry = self._materialize(record, turn, generation, len(batch))
                except ValueError:
                    failed += 1
                    cycle = replace(cycle, order=cycle.order[:index] + cycle.order[index + 1:],
                                    skipped=cycle.skipped + (item.id,))
                    continue
                entry["epoch"] = item.epoch
                batch.append(entry)
                turn = "remote" if entry["source"] == "local" else "local"
            prior_generation = state.get("generation")
            state = {key: value for key, value in state.items() if key != "needs_rebalance"}
            state.update(cycle=cycle, generation=generation, batch=batch,
                         batch_position=0, selection_policy="balanced-v1",
                         retired_generation=prior_generation,
                         retired_generations=list(dict.fromkeys(
                             [*state.get("retired_generations", []),
                              *([state["retired_generation"]] if state.get("retired_generation") else []),
                              *([prior_generation] if prior_generation else [])])))
            self._save(state)
            committed = True
            for entry in retired:
                file = entry.get("file", "")
                if (Path(file).parts[:1] == ("downloads",)
                        and Path(file).name.startswith(f"{prior_generation}-")):
                    payload = self.next_dir / file
                    if payload.is_file() and not payload.is_symlink():
                        payload.unlink()
            return state
        except Exception:
            if not committed:
                shutil.rmtree(folder)
                for path in (self.next_dir / "downloads").glob(f"{generation}-*"):
                    if path.is_file() and not path.is_symlink():
                        path.unlink()
            raise

    def _stage_refill(self, records: dict[str, dict], state: dict) -> tuple[dict, Path]:
        # Objetivo: preparar faltantes sin bloquear el atajo ni tocar el cursor visible.
        generation = f"batch-{uuid4().hex}"
        folder = self.next_dir / generation
        folder.mkdir()
        cycle = state["cycle"]
        pending = state["batch"][state["batch_position"]:]
        batch = []
        failed = 0
        turn = cycle.source_turn
        try:
            for entry in pending:
                source = self.next_dir / entry["file"]
                target = folder / f"{len(batch):04}{source.suffix}"
                os.link(source, target)
                batch.append({**entry, "file": f"{generation}/{target.name}"})
            while len(batch) < self.batch_size:
                if failed > len(records) * 2:
                    raise ValueError("no hay fuentes suficientes para completar el lote")
                cycle = ensure_upcoming(cycle, list(records), self.batch_size, self.rng)
                index = cycle.position + len(batch)
                epoch = cycle.order[index].epoch
                preferred = next((candidate for candidate in range(index, len(cycle.order))
                                  if cycle.order[candidate].epoch == epoch
                                  and (record := records.get(cycle.order[candidate].id))
                                  and record[turn]), None)
                if preferred is not None and preferred != index:
                    ordered = list(cycle.order)
                    ordered[index], ordered[preferred] = ordered[preferred], ordered[index]
                    cycle = replace(cycle, order=tuple(ordered))
                item = cycle.order[index]
                record = records.get(item.id)
                try:
                    if record is None:
                        raise ValueError("contenido ausente del índice")
                    entry = self._materialize(record, turn, generation, len(batch))
                except ValueError:
                    failed += 1
                    cycle = replace(cycle, order=cycle.order[:index] + cycle.order[index + 1:],
                                    skipped=cycle.skipped + (item.id,))
                    continue
                entry["epoch"] = item.epoch
                batch.append(entry)
                turn = "remote" if entry["source"] == "local" else "local"
            old = state.get("generation")
            retired = list(dict.fromkeys(
                [*state.get("retired_generations", []),
                 *([state["retired_generation"]] if state.get("retired_generation") else []),
                 *([old] if old else [])]))
            next_state = {key: value for key, value in state.items() if key != "needs_rebalance"}
            next_state.update(cycle=cycle, generation=generation, batch=batch,
                              batch_position=0, selection_policy="balanced-v1",
                              retired_generation=None, retired_generations=retired)
            return next_state, folder
        except Exception:
            shutil.rmtree(folder)
            raise

    def _unify_gallery(self, records: dict[str, dict], state: dict) -> dict:
        # Objetivo: migrar un lote preparado anterior a una sola carpeta visible por DMS.
        generation = state["generation"]
        if not generation or not GENERATION.fullmatch(generation):
            return state
        batch = [dict(entry) for entry in state["batch"]]
        changed = False
        for slot, entry in enumerate(batch):
            source = self.next_dir / entry["file"]
            target = self.next_dir / generation / f"{slot:04}{source.suffix}"
            if source == target:
                continue
            record = records.get(entry["id"])
            if not source.is_file():
                if record is None:
                    raise ValueError(f"no se puede recuperar imagen usada {entry['id']}")
                recovered = self._materialize(record, entry["source"], generation, slot)
                entry.update(file=recovered["file"], source=recovered["source"])
                changed = True
                continue
            if not target.exists():
                os.link(source, target)
            if record is not None:
                _verify_file(target, record)
            entry["file"] = f"{generation}/{target.name}"
            changed = True
        if changed:
            state = {**state, "batch": batch}
            self._save(state)
        return state

    def _write_cache(self, selected: Path) -> None:
        # Objetivo: reflejar la ruta realmente visible mediante renombrado atómico.
        descriptor, temp_path = tempfile.mkstemp(prefix=".current-wallpaper-", dir=self.cache_file.parent)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                output.write(f"{selected}\n")
            os.replace(temp_path, self.cache_file)
        finally:
            Path(temp_path).unlink(missing_ok=True)

    def _sync_dms_selection(self, state: dict) -> dict:
        # Criterio: una miniatura elegida en DMS cuenta antes del siguiente atajo.
        current = subprocess.run([str(self.dms_bin), "ipc", "call", "wallpaper", "get"],
                                 capture_output=True, text=True, check=True).stdout.strip()
        position = state["batch_position"]
        chosen = next((slot for slot in range(position, len(state["batch"]))
                       if current == str(self.next_dir / state["batch"][slot]["file"])), None)
        if chosen is None:
            return state
        for slot in range(position, chosen):
            state["cycle"] = skip_current(state["cycle"])
            state["batch_position"] += 1
        entry = state["batch"][chosen]
        state["cycle"] = mark_applied(state["cycle"], source=entry["source"])
        state["batch_position"] += 1
        self._save(state)
        self._write_cache(Path(current))
        return state

    def _run_locked(self, *, prepare_only: bool) -> int | None:
        # Objetivo: solo el servicio de preparación consulta JSONL y descarga imágenes.
        if prepare_only:
            records = {record["id"]: record for record in read_jsonl(self.next_dir / "index.jsonl")}
            state = self._load(records)
            self._prepare_locked(records, state)
            return None
        state = self._load(None)
        previous_position = state["batch_position"]
        state = self._sync_dms_selection(state)
        if state["batch_position"] >= len(state["batch"]):
            if previous_position < len(state["batch"]):
                return 0
            raise ValueError("lote agotado; espere la preparación en segundo plano")
        entry = state["batch"][state["batch_position"]]
        selected = self.next_dir / entry["file"]
        # Criterio: DMS debe usar una ruta dentro de la carpeta plana del lote.
        subprocess.run([str(self.dms_bin), "ipc", "call", "wallpaper", "set", str(selected)], check=True)
        previous_remote = state["cleanup_remote"]
        state["cycle"] = mark_applied(state["cycle"], source=entry["source"])
        state["batch_position"] += 1
        state["cleanup_remote"] = (entry["file"] if entry["file"].startswith("downloads/") else None)
        self._save(state)
        self._write_cache(selected)
        if previous_remote:
            previous = self.next_dir / previous_remote
            if (Path(previous_remote).parts[0] == "downloads" and previous.is_file()
                    and not previous.is_symlink()):
                previous.unlink()
        retired = list(dict.fromkeys(
            [*state.get("retired_generations", []),
             *([state["retired_generation"]] if state.get("retired_generation") else [])]))
        if retired:
            for generation in retired:
                if not GENERATION.fullmatch(generation):
                    continue
                old = self.next_dir / generation
                if old.is_dir() and not old.is_symlink():
                    shutil.rmtree(old)
                for path in (self.next_dir / "downloads").glob(f"{generation}-*"):
                    if path.is_file() and not path.is_symlink():
                        path.unlink()
            state["retired_generation"] = None
            state["retired_generations"] = []
            self._save(state)
        for old in self.managed_dir.glob("wallpaper-*"):
            if old.is_dir() and not old.is_symlink():
                shutil.rmtree(old)
        return len(state["batch"]) - state["batch_position"]

    def _run(self, *, prepare_only: bool) -> int | None:
        if self.next_dir.is_symlink():
            raise ValueError(".next no puede ser un enlace simbólico")
        self.next_dir.mkdir(exist_ok=True)
        self.managed_dir.mkdir(parents=True, exist_ok=True)
        with (self.managed_dir / ".lock").open("w") as lock:
            # Criterio: el atajo nunca espera una descarga que esté preparando el servicio.
            fcntl.flock(lock, fcntl.LOCK_EX if prepare_only else fcntl.LOCK_EX | fcntl.LOCK_NB)
            return self._run_locked(prepare_only=prepare_only)

    def prepare(self) -> None:
        self._run(prepare_only=True)

    def refill(self) -> None:
        # Objetivo: reponer hasta N conservando pendientes y publicar solo un lote completo.
        if self.next_dir.is_symlink():
            raise ValueError(".next no puede ser un enlace simbólico")
        self.next_dir.mkdir(exist_ok=True)
        self.managed_dir.mkdir(parents=True, exist_ok=True)
        lock_path = self.managed_dir / ".lock"
        with (self.managed_dir / ".refill.lock").open("w") as refill_lock:
            # Criterio: dos disparadores reponen una sola vez sin bloquear el rotador.
            fcntl.flock(refill_lock, fcntl.LOCK_EX)
            for _ in range(3):
                with lock_path.open("w") as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    records = {record["id"]: record for record in read_jsonl(self.next_dir / "index.jsonl")}
                    state = self._load(records)
                    if len(state["batch"]) - state["batch_position"] >= self.batch_size:
                        return
                    cursor = self.next_dir / ".cursor.json"
                    snapshot = cursor.read_bytes() if cursor.exists() else None
                staged, folder = self._stage_refill(records, state)
                with lock_path.open("w") as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX)
                    current = cursor.read_bytes() if cursor.exists() else None
                    if current == snapshot:
                        try:
                            self._save(staged)
                        except Exception:
                            shutil.rmtree(folder)
                            raise
                        return
                shutil.rmtree(folder)
        raise RuntimeError("cursor modificado durante la reposición; reintente")

    def rotate(self) -> int:
        remaining = self._run(prepare_only=False)
        assert remaining is not None
        return remaining

    def rebind_current(self) -> None:
        # Objetivo: apuntar DMS al lote existente sin cambiar imagen ni consumir cursor.
        self.managed_dir.mkdir(parents=True, exist_ok=True)
        with (self.managed_dir / ".lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            state = self._load(None)
            if state["batch_position"] < 1:
                raise ValueError("no hay fondo actual dentro del lote")
            selected = self.next_dir / state["batch"][state["batch_position"] - 1]["file"]
            current = subprocess.run([str(self.dms_bin), "ipc", "call", "wallpaper", "get"],
                                     capture_output=True, text=True, check=True).stdout.strip()
            if (not current or not Path(current).is_file()
                    or not filecmp.cmp(current, selected, shallow=False)):
                raise ValueError("el fondo actual de DMS no coincide con el lote")
            subprocess.run([str(self.dms_bin), "ipc", "call", "wallpaper", "set", str(selected)], check=True)
            self._write_cache(selected)
            for old in self.managed_dir.glob("wallpaper-*"):
                if old.is_dir() and not old.is_symlink():
                    shutil.rmtree(old)

    def skip_and_prepare(self) -> int:
        # Objetivo: publicar otro lote sin consumir pendientes ni borrar la galería visible.
        if self.next_dir.is_symlink():
            raise ValueError(".next no puede ser un enlace simbólico")
        self.managed_dir.mkdir(parents=True, exist_ok=True)
        with (self.managed_dir / ".lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            records = {record["id"]: record for record in read_jsonl(self.next_dir / "index.jsonl")}
            state = self._load(records)
            pending = len(state["batch"]) - state["batch_position"]
            next_state = {**state}
            for _ in range(pending):
                next_state["cycle"] = skip_current(next_state["cycle"])
                next_state["batch_position"] += 1
            self._prepare_locked(records, next_state)
            return pending
