"""Contrato del único punto de configuración y migración del estado existente."""

from pathlib import Path
import json
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.config import load_config, migrate_legacy  # noqa: E402


class ConfigTests(unittest.TestCase):
    def test_config_can_live_outside_state_directory(self):
        # Criterio CFG-SEPARATE: cargar opciones del proyecto sin mover datos de rotación.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / "wallpapers" / ".next"
            state.mkdir(parents=True)
            config_path = base / "project" / "config.toml"
            config_path.parent.mkdir()
            config_path.write_text(
                'repositories = ["https://github.com/u/r.git"]\nbatch_size = 16\n')
            config = load_config(state.parent, config_path=config_path)
            self.assertEqual(config.batch_size, 16)
            self.assertFalse((state / "config.toml").exists())

    def test_config_contains_repositories_and_batch_size(self):
        # Criterio CFG-ONE: repositorios y tamaño del lote salen de un solo TOML.
        with tempfile.TemporaryDirectory() as root:
            state = Path(root) / ".next"
            state.mkdir()
            (state / "config.toml").write_text(
                'repositories = ["https://github.com/u/r.git"]\nbatch_size = 7\n')
            config = load_config(Path(root))
            self.assertEqual(config.repositories, ("https://github.com/u/r.git",))
            self.assertEqual(config.batch_size, 7)

    def test_migration_preserves_pending_cursor_and_files(self):
        # Criterio CFG-MOVE: mover el lote y su posición sin descargar ni consumir imágenes.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            (state / "batch-abc").mkdir(parents=True)
            (state / "batch-abc" / "0000.jpg").write_bytes(b"image")
            cursor = {"version": 2, "generation": "batch-abc", "batch_position": 0,
                      "batch": [{"file": "batch-abc/0000.jpg"}]}
            (state / ".cursor.json").write_text(json.dumps(cursor))
            (base / "index").mkdir()
            (base / "index" / "index.jsonl").write_text("catalog\n")
            (base / "index" / "legacy.meta.json").write_text("legacy\n")
            (base / "repositories").mkdir()
            (base / "repositories" / "repos.toml").write_text(
                'repositories = ["https://github.com/u/r.git"]\n')
            migrate_legacy(base)
            self.assertEqual(json.loads((state / ".cursor.json").read_text()), cursor)
            self.assertEqual((state / "batch-abc" / "0000.jpg").read_bytes(), b"image")
            self.assertEqual((state / "index.jsonl").read_text(), "catalog\n")
            self.assertEqual(load_config(base).batch_size, 10)
            self.assertFalse((base / "repositories" / "repos.toml").exists())
            self.assertEqual((state / ".migration-backup" / "repos.toml").read_text(),
                             'repositories = ["https://github.com/u/r.git"]\n')
            self.assertEqual((state / ".migration-backup" / "index.jsonl").read_text(), "catalog\n")
            self.assertEqual((state / ".migration-backup" / "legacy-index" /
                              "legacy.meta.json").read_text(), "legacy\n")
            self.assertFalse((base / "index").exists())
            self.assertFalse((base / "repositories").exists())

    def test_invalid_batch_size_fails(self):
        # Criterio CFG-VALID: rechazar lotes fuera del rango antes de cambiar el cursor.
        with tempfile.TemporaryDirectory() as root:
            state = Path(root) / ".next"
            state.mkdir()
            (state / "config.toml").write_text(
                'repositories = ["https://github.com/u/r.git"]\nbatch_size = 0\n')
            with self.assertRaises(ValueError):
                load_config(Path(root))
