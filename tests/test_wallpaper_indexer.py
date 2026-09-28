"""Pruebas del CLI que mantiene el JSONL fuera del temporizador."""

import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.catalog import git_blob_id, read_jsonl, write_jsonl  # noqa: E402
from wallpaper_rotation.indexer import refresh  # noqa: E402


def repository_fetch(calls, *, fail_repository=None):
    # Criterio I-FAKE: simular árboles por repo y poder fallar en un punto determinista.
    def fetch(url):
        calls.append(url)
        parts = url.rstrip("/").split("/")
        repository = "/".join(parts[parts.index("repos") + 1:parts.index("repos") + 3]) \
            if "repos" in parts else None
        if repository == fail_repository and url.endswith(f"/repos/{repository}"):
            raise OSError("red de prueba")
        if any(url.endswith(f"/repos/u/{name}") for name in ("a", "b", "c")):
            return {"default_branch": "main"}
        if "/commits/" in url:
            return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
        payload = f"image:{repository}".encode()
        return {"truncated": False, "tree": [{"type": "blob", "path": "wall.jpg",
                "sha": git_blob_id(payload), "size": len(payload)}]}
    return fetch


class IndexerTests(unittest.TestCase):
    def test_refresh_queries_only_repositories_absent_from_index(self):
        # Criterio I-PENDING: conservar las entradas previas y consultar solo el repo pendiente.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            state.mkdir()
            config = state / "config.toml"
            config.write_text('repositories = ["https://github.com/u/a.git", '
                              '"https://github.com/u/b.git"]\nbatch_size = 10\n')
            old = b"old image"
            write_jsonl(state / "index.jsonl", [{"id": git_blob_id(old), "size": len(old),
                "local": [], "remote": [{"url": "https://raw.githubusercontent.com/u/a/" +
                "a" * 40 + "/old.jpg", "repository": "u/a", "path": "old.jpg"}]}])
            calls = []
            count = refresh(base, repository_fetch(calls), config_path=config)
            self.assertEqual(count, 2)
            self.assertEqual({url.split("/repos/")[-1].split("/")[0:2][0] for url in calls
                              if "/repos/" in url}, {"u"})
            self.assertTrue(all("/repos/u/b" in url for url in calls))
            records = read_jsonl(state / "index.jsonl")
            repositories = {source["repository"] for row in records for source in row["remote"]}
            self.assertEqual(repositories, {"u/a", "u/b"})

    def test_refresh_resumes_after_a_repository_failure(self):
        # Criterio I-RESUME: persistir cada repo exitoso y reintentar solo el que falló.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            state.mkdir()
            config = state / "config.toml"
            config.write_text('repositories = ["https://github.com/u/a.git", '
                              '"https://github.com/u/b.git"]\nbatch_size = 10\n')
            first_calls = []
            with self.assertRaisesRegex(ValueError, "u/b"):
                refresh(base, repository_fetch(first_calls, fail_repository="u/b"),
                        config_path=config)
            first_records = read_jsonl(state / "index.jsonl")
            self.assertTrue(any(source["repository"] == "u/a"
                                for row in first_records for source in row["remote"]))
            self.assertEqual(json.loads((state / ".refresh-state.json").read_text())[
                "completed"], ["u/a"])

            retry_calls = []
            refresh(base, repository_fetch(retry_calls), config_path=config)
            self.assertTrue(retry_calls)
            self.assertTrue(all("/repos/u/b" in url for url in retry_calls))

    def test_refresh_continues_to_later_repositories_after_one_fails(self):
        # Criterio I-CONTINUE: indexar repos accesibles después de un fallo y dejarlo pendiente.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            state.mkdir()
            config = state / "config.toml"
            config.write_text('repositories = ["https://github.com/u/a.git", '
                              '"https://github.com/u/b.git", "https://github.com/u/c.git"]\n'
                              'batch_size = 10\n')
            calls = []
            with self.assertRaisesRegex(ValueError, "u/b"):
                refresh(base, repository_fetch(calls, fail_repository="u/b"),
                        config_path=config)
            completed = json.loads((state / ".refresh-state.json").read_text())["completed"]
            self.assertEqual(completed, ["u/a", "u/c"])
            repositories = {source["repository"] for row in read_jsonl(state / "index.jsonl")
                            for source in row["remote"]}
            self.assertEqual(repositories, {"u/a", "u/c"})

    def test_refresh_checkpoints_repository_with_no_images(self):
        # Criterio I-EMPTY: no volver a consultar un repo vacío ya procesado.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            state.mkdir()
            config = state / "config.toml"
            config.write_text('repositories = ["https://github.com/u/a.git"]\n'
                              'batch_size = 10\n')
            (base / "local.jpg").write_bytes(b"local")
            calls = []

            def empty_repository(url):
                calls.append(url)
                if url.endswith("/repos/u/a"):
                    return {"default_branch": "main"}
                if "/commits/" in url:
                    return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
                return {"truncated": False, "tree": []}

            self.assertEqual(refresh(base, empty_repository, config_path=config), 1)
            self.assertEqual(json.loads((state / ".refresh-state.json").read_text())[
                "completed"], ["u/a"])
            calls.clear()
            self.assertEqual(refresh(base, empty_repository, config_path=config), 1)
            self.assertEqual(calls, [])

    def test_refresh_updates_local_entries_without_revisiting_completed_repos(self):
        # Criterio I-LOCAL: refrescar imágenes locales sin consultar repos ya indexados.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            state = base / ".next"
            state.mkdir()
            config = state / "config.toml"
            config.write_text('repositories = ["https://github.com/u/a.git"]\n'
                              'batch_size = 10\n')
            old = b"remote"
            write_jsonl(state / "index.jsonl", [{"id": git_blob_id(old), "size": len(old),
                "local": [], "remote": [{"url": "https://raw.githubusercontent.com/u/a/" +
                "a" * 40 + "/wall.jpg", "repository": "u/a", "path": "wall.jpg"}]}])
            (base / "new-local.jpg").write_bytes(b"local")
            count = refresh(base, lambda url: self.fail("repo ya indexado consultado"),
                            config_path=config)
            self.assertEqual(count, 2)
            self.assertEqual(len(read_jsonl(state / "index.jsonl")), 2)

    def test_refresh_uses_toml_and_writes_merged_catalog(self):
        # Criterio I-REFRESH: construir el JSONL manual desde el TOML sin clonar Git.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            config = base / ".next" / "config.toml"
            config.parent.mkdir()
            config.write_text('repositories = ["https://github.com/u/r.git"]\nbatch_size = 10\n')
            (base / "wall.jpg").write_bytes(b"image")

            def fetch(url):
                if url.endswith("/repos/u/r"):
                    return {"default_branch": "main"}
                if url.endswith("/commits/main"):
                    return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
                return {"truncated": False, "tree": [{"type": "blob", "path": "wall.jpg",
                           "sha": "47653eec14f7f531cd7abe4355bf30cd4d892631", "size": 5}]}

            count = refresh(base, fetch)
            self.assertEqual(count, 1)
            records = [json.loads(line) for line in (base / ".next" / "index.jsonl").read_text().splitlines()]
            self.assertEqual(len(records[0]["local"]), 1)
            self.assertEqual(len(records[0]["remote"]), 1)

    def test_failed_refresh_preserves_previous_index(self):
        # Criterio I-ATOMIC: un árbol truncado no reemplaza el JSONL vigente.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            config = base / ".next" / "config.toml"
            config.parent.mkdir()
            config.write_text('repositories = ["https://github.com/u/r.git"]\nbatch_size = 10\n')
            database = base / ".next" / "index.jsonl"
            database.write_text("previous\n")

            def fetch(url):
                if url.endswith("/repos/u/r"):
                    return {"default_branch": "main"}
                if url.endswith("/commits/main"):
                    return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
                return {"truncated": True, "tree": []}

            with self.assertRaises(ValueError):
                refresh(base, fetch)
            self.assertEqual(database.read_text(), "previous\n")


if __name__ == "__main__":
    unittest.main()
