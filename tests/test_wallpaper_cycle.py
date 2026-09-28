"""Reglas puras del ciclo global sin repetición por contenido."""

import random
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.cycle import (  # noqa: E402
    ensure_upcoming, initial_state, mark_applied, skip_current,
)


class CycleTests(unittest.TestCase):
    def test_each_content_is_used_once_before_new_cycle(self):
        # Criterio R-UNIQUE: no repetir hashes durante un ciclo completo.
        ids = ["a", "b", "c"]
        state = initial_state(ids, None, random.Random(1))
        seen = []
        for _ in range(3):
            state = ensure_upcoming(state, ids, 2, random.Random(2))
            seen.append(state.order[state.position].id)
            state = mark_applied(state)
        self.assertEqual(set(seen), set(ids))
        self.assertEqual(state.current_id, seen[-1])
        state = ensure_upcoming(state, ids, 2, random.Random(3))
        self.assertNotEqual(state.order[state.position].id, seen[-1])

    def test_remote_failure_skips_id_without_changing_current(self):
        # Criterio R-OFFLINE: avanzar la URL fallida sin afirmar que se mostró.
        state = initial_state(["a", "b"], "prior", random.Random(1))
        failed = state.order[0].id
        state = skip_current(state)
        self.assertEqual(state.position, 1)
        self.assertEqual(state.skipped, (failed,))
        self.assertEqual(state.current_id, "prior")

    def test_tail_can_prepare_ten_across_cycle_boundary(self):
        # Criterio R-BATCH: mantener diez disponibles sin repetir antes del límite de ciclo.
        ids = [str(number) for number in range(12)]
        state = initial_state(ids, None, random.Random(1))
        for _ in range(11):
            state = mark_applied(state)
        state = ensure_upcoming(state, ids, 10, random.Random(2))
        upcoming = state.order[state.position:state.position + 10]
        self.assertEqual(len(upcoming), 10)
        self.assertEqual(upcoming[0].epoch, 0)
        self.assertEqual(upcoming[1].epoch, 1)

    def test_source_turn_tracks_last_applied_source(self):
        # Criterio R-MIX: el siguiente turno usa la procedencia opuesta a la recién aplicada.
        state = initial_state(["a", "b"], None, random.Random(1))
        state = mark_applied(state, source="local")
        self.assertEqual(state.source_turn, "remote")
        state = mark_applied(state, source="remote")
        self.assertEqual(state.source_turn, "local")


if __name__ == "__main__":
    unittest.main()
