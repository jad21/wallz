"""Pruebas de integración del cron con JSONL, cursor, DMS y descargas."""

import json
import os
from pathlib import Path
import random
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.catalog import git_blob_id, write_jsonl  # noqa: E402
from wallpaper_rotation.engine import WallpaperEngine  # noqa: E402


class EngineTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.wallpapers = Path(self.work.name) / "wallpaper"
        self.wallpapers.mkdir()
        self.cache = Path(self.work.name) / "cache" / "current-wallpaper"
        self.log = Path(self.work.name) / "dms.log"
        self.dms = Path(self.work.name) / "dms"
        self.dms.write_text("#!/bin/sh\n[ \"${DMS_FAIL:-0}\" = 0 ] || exit 7\n"
                            "if [ \"$4\" = get ]; then cat \"$DMS_CURRENT\" 2>/dev/null; exit 0; fi\n"
                            "printf '%s\\n' \"$5\" >> \"$DMS_LOG\"\n"
                            "printf '%s\\n' \"$5\" > \"$DMS_CURRENT\"\n")
        self.dms.chmod(0o755)
        self.current = Path(self.work.name) / "dms-current"
        os.environ["DMS_CURRENT"] = str(self.current)
        self.addCleanup(os.environ.pop, "DMS_CURRENT", None)
        self.content = {}
        records = []
        for number in range(5):
            data = f"image-{number}".encode()
            digest = git_blob_id(data)
            self.content[digest] = data
            local = []
            remote = []
            if number < 4:
                path = self.wallpapers / f"{number}.jpg"
                path.write_bytes(data)
                local.append({"kind": "file", "path": str(path)})
            if number >= 3:
                remote.append({"url": f"https://raw.githubusercontent.com/u/r/{'a'*40}/{number}.jpg",
                               "repository": "u/r", "path": f"{number}.jpg"})
            records.append({"id": digest, "size": len(data), "local": local, "remote": remote})
        write_jsonl(self.wallpapers / ".next" / "index.jsonl", records)

    def engine(self, *, fail_download=False):
        def download(record, source, target):
            if fail_download:
                raise OSError("offline")
            target.write_bytes(self.content[record["id"]])

        return WallpaperEngine(self.wallpapers, self.cache, self.dms,
                               batch_size=2, rng=random.Random(1), downloader=download)

    def state(self):
        return json.loads((self.wallpapers / ".next" / ".cursor.json").read_text())

    def test_prepare_and_cycle_do_not_repeat_content(self):
        # Criterio E-UNIQUE: diez lotes pueden avanzar sin repetir hashes hasta agotar el ciclo.
        engine = self.engine()
        engine.prepare()
        self.assertEqual(self.state()["version"], 2)
        self.assertEqual(len(self.state()["batch"]), 2)
        self.assertFalse(self.log.exists())
        seen = []
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        for _ in range(5):
            if self.state()["batch_position"] == len(self.state()["batch"]):
                engine.prepare()
            engine.rotate()
            state = self.state()
            seen.append(state["cycle"]["current_id"])
            self.assertEqual(Path(self.cache.read_text().strip()).read_bytes(), self.content[seen[-1]])
        self.assertEqual(len(set(seen)), 5)

    def test_prepare_alternates_sources_when_unused_local_exists(self):
        # Criterio E-MIX: buscar local más adelante en el ciclo evita un lote solo remoto.
        engine = self.engine()
        engine.batch_size = 5
        engine.prepare()
        sources = [entry["source"] for entry in self.state()["batch"]]
        self.assertEqual(sources[:4], ["local", "remote", "local", "remote"])
        folder = self.wallpapers / ".next" / self.state()["generation"]
        self.assertEqual(len(list(folder.iterdir())), 5)
        self.assertTrue(all((self.wallpapers / ".next" / item["file"]).parent == folder
                            for item in self.state()["batch"]))

    def test_dms_failure_preserves_cursor(self):
        # Criterio E-DMS: un rechazo de DMS no consume la entrada preparada.
        engine = self.engine()
        engine.prepare()
        before = self.state()
        os.environ["DMS_LOG"] = str(self.log)
        os.environ["DMS_FAIL"] = "1"
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        self.addCleanup(os.environ.pop, "DMS_FAIL", None)
        with self.assertRaises(Exception):
            engine.rotate()
        self.assertEqual(self.state(), before)
        self.assertFalse(self.cache.exists())

    def test_gallery_keeps_whole_batch_visible_until_replacement(self):
        # Criterio E-GALLERY: DMS ve todas las imágenes juntas mientras el lote está activo.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        folder = self.wallpapers / ".next" / self.state()["generation"]
        engine.rotate()
        self.assertEqual(self.current.read_text().strip(), str(folder / Path(self.state()["batch"][0]["file"]).name))
        self.assertEqual(len(list(folder.iterdir())), 2)
        engine.rotate()
        self.assertEqual(len(list(folder.iterdir())), 2)
        engine.prepare()
        self.assertTrue(folder.exists())
        engine.rotate()
        self.assertFalse(folder.exists())
        self.assertTrue(all(path.exists() for path in self.wallpapers.glob("[0-9].jpg")))

    def test_dms_gallery_selection_advances_cursor_before_next_shortcut(self):
        # Criterio E-SYNC: elegir una miniatura posterior evita repetirla con el atajo.
        engine = self.engine()
        engine.batch_size = 5
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        batch = self.state()["batch"]
        chosen = self.wallpapers / ".next" / batch[3]["file"]
        self.current.write_text(f"{chosen}\n")
        self.assertEqual(engine.rotate(), 0)
        state = self.state()
        self.assertEqual(state["batch_position"], 5)
        self.assertEqual(state["cycle"]["current_id"], batch[4]["id"])
        self.assertIn(batch[1]["id"], state["cycle"]["skipped"])
        self.assertIn(batch[2]["id"], state["cycle"]["skipped"])

    def test_existing_split_batch_is_unified_without_advancing(self):
        # Criterio E-UPGRADE: migrar un lote remoto anterior sin cambiar DMS ni el cursor.
        engine = self.engine()
        engine.batch_size = 5
        engine.prepare()
        state = self.state()
        slot = next(i for i, entry in enumerate(state["batch"]) if entry["source"] == "remote")
        original = self.wallpapers / ".next" / state["batch"][slot]["file"]
        old = self.wallpapers / ".next" / "downloads" / f"legacy-{slot}{original.suffix}"
        old.parent.mkdir(exist_ok=True)
        original.rename(old)
        old.unlink()
        state["batch"][slot]["file"] = f"downloads/{old.name}"
        state["batch_position"] = slot + 1
        state["cycle"]["position"] = slot + 1
        (self.wallpapers / ".next" / ".cursor.json").write_text(json.dumps(state))
        engine.prepare()
        after = self.state()
        self.assertEqual(after["batch_position"], slot + 1)
        self.assertEqual(after["generation"], state["generation"])
        self.assertEqual(len(list((self.wallpapers / ".next" / state["generation"]).iterdir())), 5)
        self.assertFalse(self.log.exists())

    def test_rebind_current_to_gallery_preserves_cursor(self):
        # Criterio E-RELINK: mostrar el lote en DMS sin consumir otro fondo durante el despliegue.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        state = self.state()
        selected = self.wallpapers / ".next" / state["batch"][0]["file"]
        old = self.wallpapers / "old-cache.jpg"
        old.write_bytes(selected.read_bytes())
        self.current.write_text(f"{old}\n")
        engine.rebind_current()
        self.assertEqual(self.current.read_text().strip(), str(selected))
        self.assertEqual(self.state(), state)

    def test_skip_batch_prepares_new_size_before_replacing_gallery(self):
        # Criterio S-NEXT: omitir pendientes solo al publicar un lote nuevo de N imágenes.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        before = self.state()
        old = self.wallpapers / ".next" / before["generation"]
        engine.batch_size = 3
        skipped = engine.skip_and_prepare()
        after = self.state()
        self.assertEqual(skipped, 1)
        self.assertEqual(after["batch_position"], 0)
        self.assertEqual(len(after["batch"]), 3)
        self.assertNotEqual(after["generation"], before["generation"])
        self.assertEqual(after["cycle"]["current_id"], before["cycle"]["current_id"])
        self.assertIn(before["batch"][1]["id"], after["cycle"]["skipped"])
        self.assertTrue(old.exists())
        engine.rotate()
        self.assertFalse(old.exists())

    def test_failed_skip_preparation_keeps_old_cursor_and_gallery(self):
        # Criterio S-FAIL: un JSONL ausente no publica el salto ni toca el fondo actual.
        engine = self.engine()
        engine.prepare()
        before = self.state()
        (self.wallpapers / ".next" / "index.jsonl").unlink()
        with self.assertRaises(FileNotFoundError):
            engine.skip_and_prepare()
        self.assertEqual(self.state(), before)
        self.assertTrue((self.wallpapers / ".next" / before["generation"]).exists())

    def test_refill_keeps_pending_and_restores_target_size(self):
        # Criterio F-PARTIAL: conservar pendientes y descargar solo las plazas usadas.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        before = self.state()
        pending_id = before["batch"][1]["id"]
        engine.refill()
        after = self.state()
        self.assertEqual(len(after["batch"]), 2)
        self.assertEqual(after["batch_position"], 0)
        self.assertEqual(after["batch"][0]["id"], pending_id)
        self.assertNotEqual(after["generation"], before["generation"])
        self.assertEqual(len(list((self.wallpapers / ".next" / after["generation"]).iterdir())), 2)
        self.assertEqual(self.current.read_text().strip(),
                         str(self.wallpapers / ".next" / before["batch"][0]["file"]))

    def test_refill_full_batch_does_not_replace_or_download(self):
        # Criterio F-FULL: una galería completa no requiere otra generación.
        engine = self.engine()
        engine.prepare()
        before = self.state()
        engine.downloader = lambda *_: self.fail("no debe descargar")
        engine.refill()
        self.assertEqual(self.state(), before)

    def test_refill_failure_preserves_cursor_and_visible_file(self):
        # Criterio F-FAIL: ningún lote parcial sustituye la galería al fallar fuentes.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        before = self.state()
        visible = Path(self.current.read_text().strip())
        (self.wallpapers / ".next" / "index.jsonl").unlink()
        with self.assertRaises(FileNotFoundError):
            engine.refill()
        self.assertEqual(self.state(), before)
        self.assertTrue(visible.exists())

    def test_refill_empty_batch_prepares_new_sixteen_without_dms(self):
        # Criterio F-EMPTY: el repositor prepara y no cambia el fondo visible.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        engine.rotate()
        before = self.current.read_text()
        engine.refill()
        self.assertEqual(len(self.state()["batch"]), 2)
        self.assertEqual(self.state()["batch_position"], 0)
        self.assertEqual(self.current.read_text(), before)

    def test_missing_database_keeps_current_untouched(self):
        # Criterio E-DB: sin índice local no se consulta GitHub ni se modifica DMS.
        (self.wallpapers / ".next" / "index.jsonl").unlink()
        with self.assertRaises(FileNotFoundError):
            self.engine().prepare()
        self.assertFalse(self.log.exists())

    def test_next_uses_only_prepared_cache_even_without_index(self):
        # Criterio E-FAST: el atajo no lee DB ni llama al descargador cuando ya existe un lote.
        engine = self.engine()
        engine.prepare()
        (self.wallpapers / ".next" / "index.jsonl").unlink()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        self.assertEqual(engine.rotate(), 1)
        self.assertEqual(self.state()["batch_position"], 1)

    def test_empty_cache_fails_fast_without_download(self):
        # Criterio E-EMPTY: el atajo no prepara ni cambia DMS al agotar el lote.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        self.assertEqual(engine.rotate(), 1)
        self.assertEqual(engine.rotate(), 0)
        before = self.state()
        with self.assertRaisesRegex(ValueError, "lote agotado"):
            engine.rotate()
        self.assertEqual(self.state(), before)

    def test_migrate_v1_cursor_preserves_backup_and_visible_wallpaper(self):
        # Criterio E-MIGRATE: sustituir el formato anterior sin perder el cursor original ni el fondo activo.
        next_dir = self.wallpapers / ".next"
        next_dir.mkdir(exist_ok=True)
        previous = {"version": 1, "position": 1, "generation": "batch-old", "entries": []}
        (next_dir / ".cursor.json").write_text(json.dumps(previous))
        self.cache.parent.mkdir()
        active = self.wallpapers / "0.jpg"
        self.cache.write_text(f"{active}\n")
        self.engine().prepare()
        self.assertEqual(json.loads((next_dir / ".cursor.v1.backup.json").read_text()), previous)
        self.assertEqual(self.cache.read_text(), f"{active}\n")
        self.assertEqual(self.state()["version"], 2)
        self.assertEqual(self.state()["cycle"]["current_id"], git_blob_id(active.read_bytes()))

    def test_cleanup_error_after_commit_keeps_new_batch(self):
        # Criterio E-COMMIT: si falla limpiar el lote anterior, el cursor nuevo nunca queda sin archivos.
        engine = self.engine()
        engine.prepare()
        os.environ["DMS_LOG"] = str(self.log)
        self.addCleanup(os.environ.pop, "DMS_LOG", None)
        engine.rotate()
        engine.rotate()
        old = self.wallpapers / ".next" / self.state()["generation"]
        engine.prepare()
        self.assertTrue(old.exists())
        real_rmtree = shutil.rmtree

        def fail_only_old(path):
            if Path(path) == old:
                raise OSError("cleanup failed")
            return real_rmtree(path)

        with patch("wallpaper_rotation.engine.shutil.rmtree", side_effect=fail_only_old):
            with self.assertRaises(OSError):
                engine.rotate()
        state = self.state()
        self.assertTrue(all((self.wallpapers / ".next" / entry["file"]).exists()
                            for entry in state["batch"]))


if __name__ == "__main__":
    unittest.main()
