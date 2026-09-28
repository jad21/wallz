package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesProjectTOML(t *testing.T) {
	// Criterio CFG-ONE: cargar repositorios y tamaño desde la configuración versionada.
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "repositories = [\n  'https://github.com/u/r.git',\n]\nbatch_size = 7\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.BatchSize != 7 || len(got.Repositories) != 1 || got.Repositories[0] != "https://github.com/u/r.git" {
		t.Fatalf("config inesperada: %+v", got)
	}
}

func TestLoadRejectsInvalidBatchSize(t *testing.T) {
	// Criterio CFG-VALID: rechazar tamaños fuera del rango antes de iniciar efectos.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("repositories = ['https://github.com/u/r']\nbatch_size = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("se esperaba rechazo del tamaño inválido")
	}
}

func TestMigrateLegacyPreservesCursorAndBacksUpOldFiles(t *testing.T) {
	// Criterio CFG-MOVE: migrar configuración e índice sin consumir ni alterar el cursor.
	root := t.TempDir()
	state := filepath.Join(root, ".next")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	cursor := []byte(`{"version":2,"generation":"batch-old","batch_position":0,"batch":[{"file":"batch-old/0000.jpg"}]}`)
	if err := os.WriteFile(filepath.Join(state, ".cursor.json"), cursor, 0o600); err != nil {
		t.Fatal(err)
	}
	oldConfig := filepath.Join(root, "repositories", "repos.toml")
	oldIndex := filepath.Join(root, "index", "index.jsonl")
	if err := os.MkdirAll(filepath.Dir(oldConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(oldIndex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldConfig, []byte("repositories = ['https://github.com/u/r.git']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldIndex, []byte("catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacy(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(state, ".cursor.json"))
	if err != nil || string(got) != string(cursor) {
		t.Fatalf("cursor alterado: %q err=%v", got, err)
	}
	if _, err := os.Stat(oldConfig); !os.IsNotExist(err) {
		t.Fatalf("configuración antigua no retirada: %v", err)
	}
	backup, err := os.ReadFile(filepath.Join(state, ".migration-backup", "repos.toml"))
	if err != nil || string(backup) != "repositories = ['https://github.com/u/r.git']\n" {
		t.Fatalf("respaldo inesperado %q err=%v", backup, err)
	}
	if _, err := Load(filepath.Join(state, "config.toml")); err != nil {
		t.Fatal(err)
	}
}
