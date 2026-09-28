package rotation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jad21/wallz/internal/catalog"
)

func TestPrepareAndRotateCommitOnlyAfterDMSAccepts(t *testing.T) {
	// Criterio E-DMS: preparar el lote sin DMS y consumirlo solo tras aplicar el fondo.
	root := t.TempDir()
	wallpapers := filepath.Join(root, "wallpapers")
	if err := os.MkdirAll(wallpapers, 0o755); err != nil {
		t.Fatal(err)
	}
	images := [][]byte{[]byte("image-0"), []byte("image-1"), []byte("image-2")}
	records := []catalog.SourceRecord{}
	for i, data := range images {
		path := filepath.Join(wallpapers, string(rune('0'+i))+".jpg")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		records = append(records, catalog.SourceRecord{ID: catalog.GitBlobID(data), Size: int64(len(data)), Local: []catalog.Source{{Kind: "file", Path: path}}})
	}
	if err := catalog.WriteJSONL(filepath.Join(wallpapers, ".next", "index.jsonl"), records); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "dms-current")
	dms := fakeDMS(t, root, current, false)
	engine, err := New(Options{WallpaperDir: wallpapers, CacheFile: filepath.Join(root, "cache", "current-wallpaper"), DMSPath: dms, BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(); err != nil {
		t.Fatal(err)
	}
	before := readState(t, filepath.Join(wallpapers, ".next", ".cursor.json"))
	if before.BatchPosition != 0 || len(before.Batch) != 2 {
		t.Fatalf("lote inicial inesperado: %+v", before)
	}
	if _, err := os.Stat(current); !os.IsNotExist(err) {
		t.Fatal("Prepare no debe modificar DMS")
	}
	if _, err := engine.Rotate(); err != nil {
		t.Fatal(err)
	}
	after := readState(t, filepath.Join(wallpapers, ".next", ".cursor.json"))
	if after.BatchPosition != 1 {
		t.Fatalf("posición no avanzó: %d", after.BatchPosition)
	}
}

func TestDMSErrorLeavesCursorUnconsumed(t *testing.T) {
	// Criterio E-REJECT: un rechazo del compositor no consume ni publica el elemento.
	root := t.TempDir()
	wallpapers := filepath.Join(root, "wallpapers")
	if err := os.MkdirAll(wallpapers, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("image")
	path := filepath.Join(wallpapers, "a.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := catalog.WriteJSONL(filepath.Join(wallpapers, ".next", "index.jsonl"), []catalog.SourceRecord{{ID: catalog.GitBlobID(data), Size: int64(len(data)), Local: []catalog.Source{{Kind: "file", Path: path}}}}); err != nil {
		t.Fatal(err)
	}
	dms := fakeDMS(t, root, filepath.Join(root, "current"), true)
	engine, err := New(Options{WallpaperDir: wallpapers, CacheFile: filepath.Join(root, "cache", "current"), DMSPath: dms, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(); err != nil {
		t.Fatal(err)
	}
	cursor := filepath.Join(wallpapers, ".next", ".cursor.json")
	before, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Rotate(); err == nil {
		t.Fatal("se esperaba error del compositor")
	}
	after, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("el cursor avanzó pese al rechazo")
	}
}

func TestFailedMaterializationDoesNotPublishPartialBatch(t *testing.T) {
	// Criterio E-ATOMIC: sin fuentes verificables no se publica una generación incompleta.
	root := t.TempDir()
	wallpapers := filepath.Join(root, "wallpapers")
	if err := os.MkdirAll(filepath.Join(wallpapers, ".next"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("missing")
	record := catalog.SourceRecord{ID: catalog.GitBlobID(data), Size: int64(len(data)), Local: []catalog.Source{{Kind: "file", Path: filepath.Join(root, "absent.jpg")}}}
	if err := catalog.WriteJSONL(filepath.Join(wallpapers, ".next", "index.jsonl"), []catalog.SourceRecord{record}); err != nil {
		t.Fatal(err)
	}
	engine, err := New(Options{WallpaperDir: wallpapers, CacheFile: filepath.Join(root, "cache", "current"), DMSPath: "/bin/false", BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(); err == nil {
		t.Fatal("se esperaba fallo de materialización")
	}
	if _, err := os.Stat(filepath.Join(wallpapers, ".next", ".cursor.json")); !os.IsNotExist(err) {
		t.Fatal("se publicó cursor parcial")
	}
	entries, err := os.ReadDir(filepath.Join(wallpapers, ".next"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "index.jsonl" && entry.Name() != "downloads" {
			t.Fatalf("residuo de generación: %s", entry.Name())
		}
	}
}

func fakeDMS(t *testing.T, dir, current string, fail bool) string {
	t.Helper()
	path := filepath.Join(dir, "dms")
	body := "#!/bin/sh\n"
	if fail {
		body += "exit 7\n"
	} else {
		body += "if [ \"$4\" = get ]; then cat \"$DMS_CURRENT\" 2>/dev/null; exit 0; fi\nprintf '%s\\n' \"$5\" > \"$DMS_CURRENT\"\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DMS_CURRENT", current)
	return path
}

func readState(t *testing.T, path string) State {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
