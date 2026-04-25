package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ListFilesTool struct {
	allowedDirs []string
}

type listFilesInput struct {
	Path string `json:"path"`
}

func NewListFilesTool(allowedDirs []string) (*ListFilesTool, error) {
	normalized := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		abs, err := ResolvePath(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed dir %q: %w", dir, err)
		}
		normalized = append(normalized, abs)
	}
	if len(normalized) == 0 {
		return nil, errors.New("at least one allowed directory is required")
	}
	return &ListFilesTool{allowedDirs: normalized}, nil
}

func (t *ListFilesTool) Name() string {
	return "list_files"
}

func (t *ListFilesTool) Description() string {
	return "Lists files in an allowed directory without showing hidden entries."
}

func (t *ListFilesTool) Execute(input json.RawMessage) (string, error) {
	var req listFilesInput
	if err := json.Unmarshal(input, &req); err != nil {
		var pathString string
		if stringErr := json.Unmarshal(input, &pathString); stringErr != nil {
			return "", errors.New("invalid list_files input")
		}
		req.Path = pathString
	}
	if strings.TrimSpace(req.Path) == "" {
		pathValue, err := extractPathAlias(input, "path", "dir", "directory")
		if err != nil {
			return "", errors.New("invalid list_files input")
		}
		req.Path = pathValue
	}

	pathValue := strings.TrimSpace(req.Path)
	if pathValue == "" {
		return "", errors.New("path is required")
	}

	absPath, err := ResolvePath(pathValue)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}

	if !isPathAllowed(absPath, t.allowedDirs) {
		return "", errors.New("path is outside allowed directories")
	}
	if isHiddenPath(absPath) {
		return "", errors.New("hidden paths are not allowed")
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", fmt.Errorf("list path: %w", err)
	}

	items := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			items = append(items, name+"/")
			continue
		}
		items = append(items, name)
	}
	sort.Strings(items)

	if len(items) == 0 {
		return fmt.Sprintf("No visible files found in %s", absPath), nil
	}
	return fmt.Sprintf("Visible entries in %s:\n- %s", absPath, strings.Join(items, "\n- ")), nil
}

func extractPathAlias(raw json.RawMessage, aliases ...string) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", err
	}
	for _, alias := range aliases {
		value, ok := payload[alias]
		if !ok {
			continue
		}
		asText, ok := value.(string)
		if !ok {
			continue
		}
		asText = strings.TrimSpace(asText)
		if asText != "" {
			return asText, nil
		}
	}
	return "", errors.New("missing path")
}

func isPathAllowed(path string, allowedDirs []string) bool {
	for _, allowedDir := range allowedDirs {
		if path == allowedDir {
			return true
		}
		prefix := allowedDir + string(os.PathSeparator)
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func isHiddenPath(path string) bool {
	cleaned := filepath.Clean(path)
	parts := strings.Split(cleaned, string(os.PathSeparator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}
