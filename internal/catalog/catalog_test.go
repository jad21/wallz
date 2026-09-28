package catalog

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitBlobIDMatchesGitObjectFormat(t *testing.T) {
	// Criterio C-HASH: producir la identidad exacta usada por el árbol GitHub.
	if got := GitBlobID([]byte("image")); got != "47653eec14f7f531cd7abe4355bf30cd4d892631" {
		t.Fatalf("hash inesperado: %s", got)
	}
}

func TestMergeDeduplicatesLocalAndRemoteSources(t *testing.T) {
	// Criterio C-DEDUP: una imagen idéntica conserva ambos orígenes en un registro.
	id := GitBlobID([]byte("image"))
	got, err := Merge([]SourceRecord{{ID: id, Size: 5, Local: []Source{{Kind: "file", Path: "/tmp/a.jpg"}}}},
		[]SourceRecord{{ID: id, Size: 5, Remote: []Source{{URL: "https://raw.githubusercontent.com/u/r/" + repeat("a", 40) + "/a.jpg"}}}})
	if err != nil || len(got) != 1 || len(got[0].Local) != 1 || len(got[0].Remote) != 1 {
		t.Fatalf("merge inesperado: %#v error=%v", got, err)
	}
}

func TestLocalCatalogReadsZipMemberWithoutExtractingArchive(t *testing.T) {
	// Criterio C-EXTRACT: leer solo imágenes seguras del ZIP y conservar intacto el original.
	dir := t.TempDir()
	archive := filepath.Join(dir, "album.zip")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	entry, err := w.Create("set/wall.jpg")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("image"))
	_, _ = w.Create("../escape.jpg")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := LocalEntries(dir)
	if err != nil || len(entries) != 1 || entries[0].ID != GitBlobID([]byte("image")) {
		t.Fatalf("catálogo inesperado: %#v error=%v", entries, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("se alteró el archivo origen: %v", err)
	}
}

func TestParseRepositoryRejectsOtherHosts(t *testing.T) {
	// Criterio C-SAFE: limitar el indexado a URLs de repositorios GitHub válidas.
	if _, _, err := ParseRepositoryURL("https://example.com/u/r.git"); err == nil {
		t.Fatal("se aceptó un host externo a GitHub")
	}
}

func TestRemoteEntriesPinsURLsToResolvedCommit(t *testing.T) {
	// Criterio C-GIT: aceptar imágenes de blobs completos y fijar sus URLs al commit consultado.
	fetch := func(raw string) (map[string]any, error) {
		switch {
		case strings.HasSuffix(raw, "/repos/u/r"):
			return map[string]any{"default_branch": "main"}, nil
		case strings.HasSuffix(raw, "/commits/main"):
			return map[string]any{"sha": repeat("a", 40), "commit": map[string]any{"tree": map[string]any{"sha": repeat("d", 40)}}}, nil
		default:
			return map[string]any{"truncated": false, "tree": []any{map[string]any{"type": "blob", "path": "folder/wall.jpg", "sha": repeat("b", 40), "size": float64(5)}, map[string]any{"type": "blob", "path": "readme.txt", "sha": repeat("c", 40), "size": float64(2)}}}, nil
		}
	}
	got, err := RemoteEntries([]string{"https://github.com/u/r.git"}, fetch)
	if err != nil || len(got) != 1 || got[0].ID != repeat("b", 40) || !strings.Contains(got[0].Remote[0].URL, "/"+repeat("a", 40)+"/folder/wall.jpg") {
		t.Fatalf("remotos=%#v err=%v", got, err)
	}
}

func TestRemoteEntriesRejectsTruncatedTree(t *testing.T) {
	// Criterio C-TRUNCATED: no aceptar catálogos incompletos que puedan borrar contenido válido.
	fetch := func(raw string) (map[string]any, error) {
		if strings.HasSuffix(raw, "/repos/u/r") {
			return map[string]any{"default_branch": "main"}, nil
		}
		if strings.Contains(raw, "/commits/") {
			return map[string]any{"sha": repeat("a", 40), "commit": map[string]any{"tree": map[string]any{"sha": repeat("d", 40)}}}, nil
		}
		return map[string]any{"truncated": true, "tree": []any{}}, nil
	}
	if _, err := RemoteEntries([]string{"https://github.com/u/r"}, fetch); err == nil {
		t.Fatal("se aceptó un árbol truncado")
	}
}

func repeat(value string, count int) string {
	out := ""
	for range count {
		out += value
	}
	return out
}
