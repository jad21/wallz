package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var gitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// FetchJSON restricts API traffic and optional PAT fallback to api.github.com.
func FetchJSON(rawURL string) (map[string]any, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "api.github.com" {
		return nil, fmt.Errorf("solo se permite la API HTTPS de GitHub")
	}
	token, err := githubToken()
	if err != nil {
		return nil, err
	}
	request := func(withToken bool) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "jad21-wallpaper-index/1")
		if withToken && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
				return fmt.Errorf("redirección GitHub inesperada")
			}
			return nil
		}}
		return client.Do(req)
	}
	response, err := request(true)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized && token != "" {
		response.Body.Close()
		response, err = request(false)
		if err != nil {
			return nil, err
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API respondió %s", response.Status)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 10*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(content) > 10*1024*1024 {
		return nil, fmt.Errorf("respuesta GitHub demasiado grande")
	}
	var result map[string]any
	if err := json.Unmarshal(content, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("respuesta GitHub inválida")
	}
	return result, nil
}

func githubToken() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	path := filepath.Join(base, "wallz", "github.json")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return "", fmt.Errorf("archivo de credenciales inválido: %s", path)
	}
	token, ok := data["github_token"].(string)
	if !ok || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("falta github_token en el archivo de credenciales: %s", path)
	}
	return strings.TrimSpace(token), nil
}

type fetcher func(string) (map[string]any, error)

// RepositoryTree resolves a public repository to a complete commit-pinned tree.
func RepositoryTree(rawURL string, fetch fetcher) (string, string, []map[string]any, error) {
	owner, repo, err := ParseRepositoryURL(rawURL)
	if err != nil {
		return "", "", nil, err
	}
	base := "https://api.github.com/repos/" + owner + "/" + repo
	info, err := fetch(base)
	if err != nil {
		return "", "", nil, err
	}
	branch, ok := info["default_branch"].(string)
	if !ok || branch == "" {
		return "", "", nil, fmt.Errorf("default_branch inválido para %s/%s", owner, repo)
	}
	commitURL := base + "/commits/" + url.PathEscape(branch)
	commit, err := fetch(commitURL)
	if err != nil {
		return "", "", nil, err
	}
	sha, _ := commit["sha"].(string)
	commitData, _ := commit["commit"].(map[string]any)
	treeData, _ := commitData["tree"].(map[string]any)
	treeSHA, _ := treeData["sha"].(string)
	if !gitSHA.MatchString(sha) || !gitSHA.MatchString(treeSHA) {
		return "", "", nil, fmt.Errorf("GitHub devolvió un SHA inválido para %s/%s", owner, repo)
	}
	tree, err := fetch(base + "/git/trees/" + treeSHA + "?recursive=1")
	if err != nil {
		return "", "", nil, err
	}
	truncated, valid := tree["truncated"].(bool)
	items, listValid := tree["tree"].([]any)
	if !valid || truncated || !listValid {
		return "", "", nil, fmt.Errorf("árbol GitHub incompleto para %s/%s", owner, repo)
	}
	entries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return "", "", nil, fmt.Errorf("entrada GitHub inválida")
		}
		entries = append(entries, entry)
	}
	return owner + "/" + repo, sha, entries, nil
}

// RemoteEntries returns public image blobs pinned to their resolved commit.
func RemoteEntries(urls []string, fetch func(string) (map[string]any, error)) ([]SourceRecord, error) {
	result := []SourceRecord{}
	for _, rawURL := range urls {
		repository, commit, tree, err := RepositoryTree(rawURL, fetch)
		if err != nil {
			return nil, err
		}
		for _, entry := range tree {
			path, _ := entry["path"].(string)
			kind, _ := entry["type"].(string)
			if kind != "blob" || !safeMember(path) || !imageExtensions[strings.ToLower(filepath.Ext(path))] {
				continue
			}
			sha, _ := entry["sha"].(string)
			sizeFloat, ok := entry["size"].(float64)
			if !gitSHA.MatchString(sha) || !ok || sizeFloat < 0 || sizeFloat != float64(int64(sizeFloat)) {
				return nil, fmt.Errorf("blob remoto inválido: %s/%s", repository, path)
			}
			imageURL := "https://raw.githubusercontent.com/" + repository + "/" + commit + "/" + strings.ReplaceAll(url.PathEscape(path), "%2F", "/")
			result = append(result, SourceRecord{ID: sha, Size: int64(sizeFloat), Remote: []Source{{URL: imageURL, Repository: repository, Path: path}}})
		}
	}
	return result, nil
}

// Folders lists folders in a repository without downloading image content.
func Folders(rawURL string, fetch func(string) (map[string]any, error)) ([]string, error) {
	_, _, tree, err := RepositoryTree(rawURL, fetch)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, entry := range tree {
		if entry["type"] == "tree" {
			if path, ok := entry["path"].(string); ok {
				result = append(result, path)
			}
		}
	}
	sortStrings(result)
	return result, nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
