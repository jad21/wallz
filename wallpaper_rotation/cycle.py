"""Estado puro del recorrido global de contenidos sin repetición."""

from dataclasses import dataclass, replace
import random


@dataclass(frozen=True)
class CycleItem:
    """Contrato: un contenido pendiente dentro de un ciclo numerado."""

    id: str
    epoch: int


@dataclass(frozen=True)
class CycleState:
    """Contrato: position apunta al siguiente contenido por aplicar o saltar."""

    order: tuple[CycleItem, ...]
    position: int
    current_id: str | None
    skipped: tuple[str, ...] = ()
    source_turn: str = "local"


def _shuffle(ids: list[str], avoid: str | None, epoch: int, rng: random.Random) -> tuple[CycleItem, ...]:
    ordered = list(dict.fromkeys(ids))
    rng.shuffle(ordered)
    if len(ordered) > 1 and ordered[0] == avoid:
        ordered.append(ordered.pop(0))
    return tuple(CycleItem(identifier, epoch) for identifier in ordered)


def initial_state(ids: list[str], current_id: str | None, rng: random.Random) -> CycleState:
    # Criterio: el fondo actual ya cuenta como usado en el nuevo ciclo, si es conocido.
    if not ids:
        raise ValueError("el catálogo no tiene imágenes")
    pending = [identifier for identifier in ids if identifier != current_id]
    return CycleState(_shuffle(pending, current_id, 0, rng), 0, current_id)


def ensure_upcoming(state: CycleState, ids: list[str], count: int, rng: random.Random) -> CycleState:
    # Criterio R-BATCH: completar el lote cruzando el límite de ciclo, nunca antes.
    if count < 1 or not ids:
        raise ValueError("no hay suficientes contenidos para preparar el lote")
    order = state.order
    while len(order) - state.position < count:
        epoch = order[-1].epoch + 1 if order else 1
        avoid = order[-1].id if order else state.current_id
        order += _shuffle(ids, avoid, epoch, rng)
    return replace(state, order=order)


def mark_applied(state: CycleState, *, source: str | None = None) -> CycleState:
    # Criterio R-UNIQUE: avanzar únicamente tras la confirmación de DMS.
    if state.position >= len(state.order):
        raise ValueError("cursor agotado")
    item = state.order[state.position]
    if source is not None and source not in {"local", "remote"}:
        raise ValueError("procedencia aplicada inválida")
    turn = ("remote" if source == "local" else "local") if source else state.source_turn
    return replace(state, position=state.position + 1, current_id=item.id, source_turn=turn)


def skip_current(state: CycleState) -> CycleState:
    # Criterio R-OFFLINE: registrar un intento fallido sin cambiar el fondo visible.
    if state.position >= len(state.order):
        raise ValueError("cursor agotado")
    item = state.order[state.position]
    return replace(state, position=state.position + 1, skipped=state.skipped + (item.id,))
