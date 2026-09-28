// Package config owns Wallz's validated, user-editable TOML contract.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Rotation is the persistent source list and prepared-batch size.
type Rotation struct {
	Repositories []string
	BatchSize    int
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Load reads the intentionally small TOML schema used by Wallz.
func Load(path string) (Rotation, error) {
	file, err := os.Open(path)
	if err != nil {
		return Rotation{}, err
	}
	defer file.Close()

	var result Rotation
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	lines := []string{}
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return Rotation{}, err
	}
	for i := 0; i < len(lines); i++ {
		line := stripComment(strings.TrimSpace(lines[i]))
		if line == "" {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return Rotation{}, fmt.Errorf("config.toml línea %d: asignación inválida", i+1)
		}
		switch strings.TrimSpace(key) {
		case "repositories":
			value := strings.TrimSpace(raw)
			for !strings.Contains(value, "]") && i+1 < len(lines) {
				i++
				value += "\n" + stripComment(lines[i])
			}
			if !strings.HasPrefix(value, "[") || !strings.HasSuffix(strings.TrimSpace(value), "]") {
				return Rotation{}, errors.New("config.toml: repositories debe ser una lista TOML")
			}
			items, err := parseStringArray(value[1:strings.LastIndex(value, "]")])
			if err != nil {
				return Rotation{}, fmt.Errorf("config.toml: repositories: %w", err)
			}
			result.Repositories = items
		case "batch_size":
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return Rotation{}, errors.New("config.toml: batch_size debe ser entero")
			}
			result.BatchSize = n
		}
	}
	if len(result.Repositories) == 0 {
		return Rotation{}, errors.New("config.toml necesita repositorios públicos únicos")
	}
	for _, repository := range result.Repositories {
		if seen[repository] {
			return Rotation{}, fmt.Errorf("config.toml: repositorio duplicado %q", repository)
		}
		seen[repository] = true
		if err := validateRepository(repository); err != nil {
			return Rotation{}, err
		}
	}
	if result.BatchSize < 1 || result.BatchSize > 100 {
		return Rotation{}, errors.New("config.toml: batch_size debe estar entre 1 y 100")
	}
	return result, nil
}

func parseStringArray(source string) ([]string, error) {
	items := []string{}
	for i := 0; i < len(source); {
		for i < len(source) && (source[i] == ' ' || source[i] == '\n' || source[i] == '\r' || source[i] == '\t' || source[i] == ',') {
			i++
		}
		if i == len(source) {
			break
		}
		quote := source[i]
		if quote != '\'' && quote != '"' {
			return nil, errors.New("se esperaba una cadena")
		}
		i++
		start := i
		for i < len(source) && source[i] != quote {
			if source[i] == '\\' && quote == '"' {
				i++
			}
			i++
		}
		if i == len(source) {
			return nil, errors.New("cadena sin cerrar")
		}
		value := source[start:i]
		i++
		if quote == '"' {
			unquoted, err := strconv.Unquote("\"" + value + "\"")
			if err != nil {
				return nil, err
			}
			value = unquoted
		}
		items = append(items, value)
	}
	return items, nil
}

func stripComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote == '"' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		} else if c == '#' {
			return line[:i]
		}
	}
	return line
}

func validateRepository(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("repositorio GitHub inválido: %s", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || !repositoryPart.MatchString(parts[0]) || !repositoryPart.MatchString(strings.TrimSuffix(parts[1], ".git")) {
		return fmt.Errorf("repositorio GitHub inválido: %s", raw)
	}
	return nil
}

// MigrateLegacy moves the retired index/config into .next while retaining recoverable backups.
func MigrateLegacy(wallpaperDir string) error {
	state := filepath.Join(wallpaperDir, ".next")
	if info, err := os.Lstat(state); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New(".next no puede ser un enlace simbólico")
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		return err
	}
	oldConfig := filepath.Join(wallpaperDir, "repositories", "repos.toml")
	oldIndex := filepath.Join(wallpaperDir, "index", "index.jsonl")
	newConfig := filepath.Join(state, "config.toml")
	if _, err := os.Stat(newConfig); os.IsNotExist(err) {
		content, err := os.ReadFile(oldConfig)
		if err != nil {
			return err
		}
		if !strings.Contains(string(content), "repositories") {
			return errors.New("repos.toml anterior inválido")
		}
		content = append(content, []byte("\nbatch_size = 10\n")...)
		if err := atomicWrite(newConfig, content); err != nil {
			return err
		}
	}
	if _, err := Load(newConfig); err != nil {
		return err
	}
	newIndex := filepath.Join(state, "index.jsonl")
	if _, err := os.Stat(newIndex); os.IsNotExist(err) {
		if err := copyAtomic(oldIndex, newIndex); err != nil {
			return err
		}
	}
	backup := filepath.Join(state, ".migration-backup")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		return err
	}
	for _, original := range []string{oldConfig, oldIndex} {
		if _, err := os.Stat(original); err == nil {
			saved := filepath.Join(backup, filepath.Base(original))
			if _, err := os.Stat(saved); os.IsNotExist(err) {
				if err := copyAtomic(original, saved); err != nil {
					return err
				}
			}
		}
	}
	if _, err := os.Stat(oldConfig); err == nil {
		if err := os.Remove(oldConfig); err != nil {
			return err
		}
	}
	if _, err := os.Stat(oldIndex); err == nil {
		if err := os.Remove(oldIndex); err != nil {
			return err
		}
	}
	oldIndexDir := filepath.Join(wallpaperDir, "index")
	if info, err := os.Lstat(oldIndexDir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		entries, err := os.ReadDir(oldIndexDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".meta.json") && !entry.IsDir() {
				legacy := filepath.Join(backup, "legacy-index")
				if err := os.MkdirAll(legacy, 0o755); err != nil {
					return err
				}
				if err := copyAtomic(filepath.Join(oldIndexDir, entry.Name()), filepath.Join(legacy, entry.Name())); err != nil {
					return err
				}
				if err := os.Remove(filepath.Join(oldIndexDir, entry.Name())); err != nil {
					return err
				}
			}
		}
		entries, err = os.ReadDir(oldIndexDir)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(oldIndexDir); err != nil {
				return err
			}
		}
	}
	oldRepoDir := filepath.Join(wallpaperDir, "repositories")
	if info, err := os.Lstat(oldRepoDir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		entries, err := os.ReadDir(oldRepoDir)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(oldRepoDir); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyAtomic(source, target string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return atomicWrite(target, content)
}
func atomicWrite(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := f.Write(content); err != nil {
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
