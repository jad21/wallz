// Package indexer updates the catalog outside the fast wallpaper-rotation path.
package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jad21/wallz/internal/catalog"
	"github.com/jad21/wallz/internal/config"
)

// Fetch is injectable so refresh behavior can be tested without network traffic.
type Fetch func(string) (map[string]any, error)

// Refresh indexes new repository trees, checkpoints successes, and leaves failed repos retryable.
func Refresh(wallpaperDir string, settings config.Rotation, fetch Fetch) (int, error) {
	stateDir := filepath.Join(wallpaperDir, ".next")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return 0, err
	}
	indexPath := filepath.Join(stateDir, "index.jsonl")
	progressPath := filepath.Join(stateDir, ".refresh-state.json")
	persisted := []catalog.SourceRecord{}
	if _, err := os.Stat(indexPath); err == nil {
		persisted, err = catalog.ReadJSONL(indexPath)
		if err != nil {
			return 0, err
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	completed, err := loadProgress(progressPath)
	if err != nil {
		return 0, err
	}
	remote := []catalog.SourceRecord{}
	for _, record := range persisted {
		for _, source := range record.Remote {
			if source.Repository != "" {
				completed[source.Repository] = true
			}
		}
		if len(record.Remote) > 0 {
			remote = append(remote, catalog.SourceRecord{ID: record.ID, Size: record.Size, Remote: record.Remote})
		}
	}
	local, err := catalog.LocalEntries(wallpaperDir)
	if err != nil {
		return 0, err
	}
	records, err := catalog.Merge(local, remote)
	if err != nil {
		return 0, err
	}
	failures := []string{}
	for _, rawURL := range settings.Repositories {
		owner, repo, err := catalog.ParseRepositoryURL(rawURL)
		if err != nil {
			return 0, err
		}
		repository := owner + "/" + repo
		if completed[repository] {
			continue
		}
		added, fetchErr := catalog.RemoteEntries([]string{rawURL}, fetch)
		if fetchErr != nil {
			failures = append(failures, repository+": "+fetchErr.Error())
			continue
		}
		remote = append(remote, added...)
		updated, mergeErr := catalog.Merge(local, remote)
		if mergeErr != nil {
			return 0, mergeErr
		}
		if len(updated) > 0 {
			if err := catalog.WriteJSONL(indexPath, updated); err != nil {
				return 0, err
			}
		}
		completed[repository] = true
		if err := saveProgress(progressPath, completed); err != nil {
			return 0, err
		}
		records = updated
	}
	if len(failures) > 0 {
		return 0, fmt.Errorf("falló el refresh de: %s", strings.Join(failures, "; "))
	}
	if len(records) == 0 {
		return 0, fmt.Errorf("no hay imágenes para indexar")
	}
	if !sameRecords(records, persisted) {
		if err := catalog.WriteJSONL(indexPath, records); err != nil {
			return 0, err
		}
	}
	return len(records), nil
}

// Folders lists GitHub tree paths and returns deterministic lexical ordering.
func Folders(rawURL string, fetch Fetch) ([]string, error) { return catalog.Folders(rawURL, fetch) }

func loadProgress(path string) (map[string]bool, error) {
	completed := map[string]bool{}
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return completed, nil
	}
	if err != nil {
		return nil, err
	}
	var data struct {
		Version   int      `json:"version"`
		Completed []string `json:"completed"`
	}
	if err := json.Unmarshal(content, &data); err != nil || data.Version != 1 || data.Completed == nil {
		return nil, fmt.Errorf(".refresh-state.json tiene un formato inválido")
	}
	for _, repository := range data.Completed {
		if _, exists := completed[repository]; exists {
			return nil, fmt.Errorf(".refresh-state.json tiene repositorios repetidos")
		}
		parts := strings.Split(repository, "/")
		if len(parts) != 2 {
			return nil, fmt.Errorf(".refresh-state.json tiene repositorios inválidos")
		}
		if _, _, err := catalog.ParseRepositoryURL("https://github.com/" + repository + ".git"); err != nil {
			return nil, err
		}
		completed[repository] = true
	}
	return completed, nil
}

func saveProgress(path string, completed map[string]bool) error {
	values := make([]string, 0, len(completed))
	for repository := range completed {
		values = append(values, repository)
	}
	sort.Strings(values)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".refresh-state-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	encErr := json.NewEncoder(f).Encode(struct {
		Version   int      `json:"version"`
		Completed []string `json:"completed"`
	}{1, values})
	if encErr != nil {
		f.Close()
		return encErr
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func sameRecords(left, right []catalog.SourceRecord) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
