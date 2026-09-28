// Package catalog owns content identity and the persistent JSONL catalog contract.
package catalog

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var imageExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".avif": true}

// Source identifies a local file/archive member or a pinned GitHub image URL.
type Source struct {
	Kind       string `json:"kind,omitempty"`
	Path       string `json:"path,omitempty"`
	Member     string `json:"member,omitempty"`
	URL        string `json:"url,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// SourceRecord deduplicates equal bytes while preserving every usable origin.
type SourceRecord struct {
	ID     string   `json:"id"`
	Size   int64    `json:"size"`
	Local  []Source `json:"local"`
	Remote []Source `json:"remote"`
}

// GitBlobID hashes bytes with Git's blob header, matching GitHub tree SHAs.
func GitBlobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// HashFile streams a file while enforcing the expected byte count.
func HashFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", size)
	buf := make([]byte, 1024*1024)
	var count int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			count += int64(n)
			h.Write(buf[:n])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", readErr
		}
	}
	if count != size {
		return "", fmt.Errorf("tamaño de imagen cambió: esperado %d, obtenido %d", size, count)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Merge deduplicates identities but rejects any impossible size collision.
func Merge(local, remote []SourceRecord) ([]SourceRecord, error) {
	merged := map[string]*SourceRecord{}
	add := func(record SourceRecord, remoteSide bool) error {
		if record.ID == "" || record.Size < 0 {
			return errors.New("registro de imagen inválido")
		}
		current := merged[record.ID]
		if current == nil {
			current = &SourceRecord{ID: record.ID, Size: record.Size, Local: []Source{}, Remote: []Source{}}
			merged[record.ID] = current
		}
		if current.Size != record.Size {
			return fmt.Errorf("hash con tamaños distintos: %s", record.ID)
		}
		if remoteSide {
			for _, source := range record.Remote {
				if !contains(current.Remote, source) {
					current.Remote = append(current.Remote, source)
				}
			}
		} else {
			for _, source := range record.Local {
				if !contains(current.Local, source) {
					current.Local = append(current.Local, source)
				}
			}
		}
		return nil
	}
	for _, record := range local {
		if err := add(record, false); err != nil {
			return nil, err
		}
	}
	for _, record := range remote {
		if err := add(record, true); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]SourceRecord, 0, len(ids))
	for _, id := range ids {
		result = append(result, *merged[id])
	}
	return result, nil
}

func contains(sources []Source, target Source) bool {
	for _, source := range sources {
		if source == target {
			return true
		}
	}
	return false
}

// WriteJSONL atomically replaces the catalog so failed writes preserve the old index.
func WriteJSONL(path string, records []SourceRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".index-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, record := range records {
		if err := enc.Encode(record); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
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

// ReadJSONL validates every identity and rejects duplicate or empty catalogs.
func ReadJSONL(path string) ([]SourceRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	result := []SourceRecord{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 12*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record SourceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("index.jsonl línea %d: %w", line, err)
		}
		if len(record.ID) != 40 || strings.Trim(record.ID, "0123456789abcdef") != "" || record.Size < 0 || seen[record.ID] || len(record.Local)+len(record.Remote) == 0 {
			return nil, fmt.Errorf("index.jsonl línea %d: registro inválido o repetido", line)
		}
		seen[record.ID] = true
		result = append(result, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, errors.New("index.jsonl está vacío")
	}
	return result, nil
}

// ParseRepositoryURL accepts only a bare HTTPS github.com owner/repository URL.
func ParseRepositoryURL(raw string) (string, string, error) {
	ownerRepo := strings.TrimSuffix(strings.TrimPrefix(raw, "https://github.com/"), ".git")
	if !strings.HasPrefix(raw, "https://github.com/") || strings.ContainsAny(raw, "?#") || strings.Contains(raw, "@") {
		return "", "", fmt.Errorf("repositorio GitHub inválido: %s", raw)
	}
	parts := strings.Split(ownerRepo, "/")
	if len(parts) != 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return "", "", fmt.Errorf("repositorio GitHub inválido: %s", raw)
	}
	return parts[0], parts[1], nil
}

func validRepositoryPart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r)) {
			return false
		}
	}
	return true
}
