"""Contratos del catálogo local/remoto y de su JSONL persistente."""

from pathlib import Path
import sys
import tempfile
import unittest
import zipfile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from wallpaper_rotation.catalog import (  # noqa: E402
    extract_local, git_blob_id, local_entries, merge_records, parse_repository_url,
    read_jsonl, remote_entries, write_jsonl,
)


class CatalogTests(unittest.TestCase):
    def test_local_and_remote_equal_bytes_share_one_record(self):
        # Criterio C-DEDUP: el mismo contenido local y remoto ocupa una sola línea.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            with zipfile.ZipFile(base / "album.zip", "w") as archive:
                archive.writestr("set/wall.jpg", b"image")
            local = local_entries(base)
            remote = [{"id": git_blob_id(b"image"), "size": 5,
                       "url": "https://raw.githubusercontent.com/u/r/abc/set/wall.jpg",
                       "repository": "u/r", "path": "set/wall.jpg"}]
            records = merge_records(local, remote)
            self.assertEqual(len(records), 1)
            self.assertEqual(len(records[0]["local"]), 1)
            self.assertEqual(len(records[0]["remote"]), 1)
            database = base / "index.jsonl"
            write_jsonl(database, records)
            self.assertEqual(read_jsonl(database), records)

    def test_public_github_tree_uses_pinned_commit_urls(self):
        # Criterio C-GIT: el índice usa blobs de imagen y URLs ligadas al commit consultado.
        calls = []

        def fetch(url):
            calls.append(url)
            if url.endswith("/repos/u/r"):
                return {"default_branch": "main"}
            if url.endswith("/repos/u/r/commits/main"):
                return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
            return {"truncated": False, "tree": [
                {"type": "blob", "path": "set/wall.jpg", "sha": "b" * 40, "size": 5},
                {"type": "blob", "path": "set/readme.txt", "sha": "c" * 40, "size": 2},
                {"type": "tree", "path": "set", "sha": "d" * 40},
            ]}

        entries = remote_entries(["https://github.com/u/r.git"], fetch)
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["id"], "b" * 40)
        self.assertIn("/" + "a" * 40 + "/set/wall.jpg", entries[0]["url"])
        self.assertEqual(len(calls), 3)

    def test_invalid_repository_and_truncated_tree_fail(self):
        # Criterio C-SAFE: rechazar hosts ajenos y árboles parciales antes de sustituir el índice.
        with self.assertRaises(ValueError):
            parse_repository_url("https://example.com/u/r.git")

        def fetch(url):
            if url.endswith("/repos/u/r"):
                return {"default_branch": "main"}
            if url.endswith("/commits/main"):
                return {"sha": "a" * 40, "commit": {"tree": {"sha": "d" * 40}}}
            return {"truncated": True, "tree": []}

        with self.assertRaises(ValueError):
            remote_entries(["https://github.com/u/r.git"], fetch)

    def test_git_blob_hash_matches_git_object_contract(self):
        # Criterio C-HASH: permitir comparación exacta con SHA de blobs de GitHub.
        self.assertEqual(git_blob_id(b"image"), "47653eec14f7f531cd7abe4355bf30cd4d892631")

    def test_extract_local_reads_only_named_zip_member(self):
        # Criterio C-EXTRACT: materializar el miembro pedido sin expandir todo el archivo.
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            archive = base / "album.zip"
            with zipfile.ZipFile(archive, "w") as zf:
                zf.writestr("set/wall.jpg", b"image")
                zf.writestr("set/other.jpg", b"other")
            target = base / "chosen.jpg"
            extract_local({"kind": "zip", "path": str(archive), "member": "set/wall.jpg"}, target)
            self.assertEqual(target.read_bytes(), b"image")
            self.assertFalse((base / "set").exists())


if __name__ == "__main__":
    unittest.main()
