"""Pruebas del comando que ejecuta el temporizador de Niri."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.catalog import local_entries, merge_records, write_jsonl


SCRIPT = Path(__file__).resolve().parents[1] / "bin" / "rotate-wallpaper.sh"


class RotateWallpaperTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.root = Path(self.work.name)
        self.wallpapers = self.root / "wallpapers"
        self.wallpapers.mkdir()
        self.cache = self.root / "cache" / "current-wallpaper"
        self.log = self.root / "dms.log"
        self.config = self.root / "config.toml"
        self.dms = self.root / "dms"
        self.dms.write_text("#!/bin/sh\n[ \"${DMS_FAIL:-0}\" = 0 ] || exit 7\nprintf '%s\\n' \"$5\" >> \"$DMS_LOG\"\n")
        self.dms.chmod(0o755)
        self.systemctl = self.root / "systemctl"
        self.systemctl.write_text("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYSTEMCTL_LOG\"\n")
        self.systemctl.chmod(0o755)
        self.systemctl_log = self.root / "systemctl.log"

    def refresh_local(self):
        # Criterio CLI-DB: la actualización del JSONL ocurre fuera del cron.
        records = merge_records(local_entries(self.wallpapers), [])
        state = self.wallpapers / ".next"
        state.mkdir(exist_ok=True)
        (state / "config.toml").write_text(
            'repositories = ["https://github.com/u/r.git"]\nbatch_size = 2\n')
        self.config.write_text((state / "config.toml").read_text())
        write_jsonl(state / "index.jsonl", records)

    def run_rotation(self, *, fail=False, prepare=False, next_batch=False):
        env = os.environ.copy()
        env.update(WALLPAPER_DIR=str(self.wallpapers), DMS_BIN=str(self.dms),
                   CACHE_FILE=str(self.cache), DMS_LOG=str(self.log),
                   WALLZ_CONFIG=str(self.config),
                   DMS_FAIL="1" if fail else "0", SYSTEMCTL_LOG=str(self.systemctl_log),
                   PATH=f"{self.root}:{env.get('PATH', '')}")
        command = "--next-batch" if next_batch else "--prepare" if prepare else "--next"
        return subprocess.run([str(SCRIPT), command],
                              env=env, capture_output=True, text=True)

    def state(self):
        return json.loads((self.wallpapers / ".next" / ".cursor.json").read_text())

    def test_prepare_then_apply_from_local_jsonl(self):
        # Criterio CLI-PREPARE: preparar no llama a DMS y rotar persiste el contenido seleccionado.
        for number in range(3):
            (self.wallpapers / f"{number}.jpg").write_bytes(f"image-{number}".encode())
        self.refresh_local()
        result = self.run_rotation(prepare=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.state()["version"], 2)
        self.assertEqual(len(self.state()["batch"]), 2)
        self.assertFalse(self.log.exists())
        result = self.run_rotation()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.state()["batch_position"], 1)
        self.assertIn(Path(self.cache.read_text().strip()).read_bytes(),
                      {b"image-0", b"image-1", b"image-2"})

    def test_archive_source_keeps_original_and_dms_failure_keeps_cursor(self):
        # Criterio CLI-SAFE: una extracción temporal no altera ZIP ni consume un rechazo de DMS.
        archive = self.wallpapers / "album.zip"
        with zipfile.ZipFile(archive, "w") as output:
            output.writestr("first.jpg", b"first")
            output.writestr("second.jpg", b"second")
        self.refresh_local()
        self.assertEqual(self.run_rotation(prepare=True).returncode, 0)
        before = self.state()
        self.assertNotEqual(self.run_rotation(fail=True).returncode, 0)
        self.assertEqual(self.state(), before)
        self.assertTrue(archive.exists())
        self.assertFalse(self.cache.exists())

    def test_missing_lot_fails_without_application(self):
        # Criterio CLI-EMPTY: el atajo no reconstruye índices ni consulta GitHub por su cuenta.
        result = self.run_rotation()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.log.exists())

    def test_last_next_requests_background_prepare(self):
        # Criterio CLI-REFILL: el último fondo pide otro lote sin descargar en el atajo.
        for number in range(2):
            (self.wallpapers / f"{number}.jpg").write_bytes(f"image-{number}".encode())
        self.refresh_local()
        self.assertEqual(self.run_rotation(prepare=True).returncode, 0)
        self.assertEqual(self.run_rotation().returncode, 0)
        self.assertFalse(self.systemctl_log.exists())
        self.assertEqual(self.run_rotation().returncode, 0)
        self.assertIn("--user start --no-block refill-wallpaper.service",
                      self.systemctl_log.read_text())
        self.assertEqual(self.state()["batch_position"], 2)

    def test_next_batch_uses_updated_size_and_applies_first(self):
        # Criterio S-CLI: cambiar N y saltar publica el lote completo antes de DMS.
        for number in range(5):
            (self.wallpapers / f"{number}.jpg").write_bytes(f"image-{number}".encode())
        self.refresh_local()
        self.assertEqual(self.run_rotation(prepare=True).returncode, 0)
        self.assertEqual(self.run_rotation().returncode, 0)
        self.config.write_text(
            'repositories = ["https://github.com/u/r.git"]\nbatch_size = 3\n')
        result = self.run_rotation(next_batch=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.state()["batch"]), 3)
        self.assertEqual(self.state()["batch_position"], 1)


if __name__ == "__main__":
    unittest.main()
