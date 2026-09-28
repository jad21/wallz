package indexer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jad21/wallz/internal/config"
)

func TestRefreshCheckpointsSuccessfulRepositoriesAcrossFailure(t *testing.T) {
	// Criterio I-RESUME: guardar cada repo exitoso y reintentar únicamente los fallidos.
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte("repositories = ['https://github.com/u/a.git', 'https://github.com/u/b.git']\nbatch_size = 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(raw string) (map[string]any, error) {
		if strings.HasSuffix(raw, "/repos/u/b") {
			return nil, errors.New("network")
		}
		if strings.HasSuffix(raw, "/repos/u/a") {
			return map[string]any{"default_branch": "main"}, nil
		}
		if strings.Contains(raw, "/commits/") {
			return map[string]any{"sha": repeat("a", 40), "commit": map[string]any{"tree": map[string]any{"sha": repeat("d", 40)}}}, nil
		}
		return map[string]any{"truncated": false, "tree": []any{}}, nil
	}
	if _, err := Refresh(root, settings, fetch); err == nil {
		t.Fatal("se esperaba error parcial")
	}
	var progress struct {
		Completed []string `json:"completed"`
	}
	data, err := os.ReadFile(filepath.Join(root, ".next", ".refresh-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		t.Fatal(err)
	}
	if len(progress.Completed) != 1 || progress.Completed[0] != "u/a" {
		t.Fatalf("checkpoint inesperado: %#v", progress)
	}
}

func TestRefreshRejectsIncompleteTreeWithoutReplacingIndex(t *testing.T) {
	// Criterio I-ATOMIC: un árbol truncado conserva el índice previamente publicado.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".next"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, ".next", "index.jsonl")
	if err := os.WriteFile(index, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := config.Rotation{Repositories: []string{"https://github.com/u/a"}, BatchSize: 10}
	fetch := func(raw string) (map[string]any, error) {
		if strings.HasSuffix(raw, "/repos/u/a") {
			return map[string]any{"default_branch": "main"}, nil
		}
		if strings.Contains(raw, "/commits/") {
			return map[string]any{"sha": repeat("a", 40), "commit": map[string]any{"tree": map[string]any{"sha": repeat("d", 40)}}}, nil
		}
		return map[string]any{"truncated": true, "tree": []any{}}, nil
	}
	if _, err := Refresh(root, settings, fetch); err == nil {
		t.Fatal("se esperaba rechazo del árbol truncado")
	}
	data, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "previous\n" {
		t.Fatalf("índice reemplazado: %q", data)
	}
}

func TestFolderOutputContainsOnlyTreeEntries(t *testing.T) {
	// Criterio I-FOLDERS: listar carpetas sin incluir imágenes ni consultar contenido.
	fetch := func(raw string) (map[string]any, error) {
		if strings.HasSuffix(raw, "/repos/u/r") {
			return map[string]any{"default_branch": "main"}, nil
		}
		if strings.Contains(raw, "/commits/") {
			return map[string]any{"sha": repeat("a", 40), "commit": map[string]any{"tree": map[string]any{"sha": repeat("d", 40)}}}, nil
		}
		return map[string]any{"truncated": false, "tree": []any{map[string]any{"type": "tree", "path": "z"}, map[string]any{"type": "blob", "path": "a.jpg"}}}, nil
	}
	got, err := Folders("https://github.com/u/r", fetch)
	if err != nil || len(got) != 1 || got[0] != "z" {
		t.Fatalf("folders=%v err=%v", got, err)
	}
}

func repeat(value string, count int) string { return strings.Repeat(value, count) }
