package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type ReadFileTool struct {
	allowedDirs []string
}

type readFileInput struct {
	Path string `json:"path"`
}

func NewReadFileTool(allowedDirs []string) *ReadFileTool {
	return &ReadFileTool{allowedDirs: allowedDirs}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Description() string {
	return "Reads UTF-8 text files from allowed directories."
}

func (t *ReadFileTool) Execute(input json.RawMessage) (string, error) {
	var req readFileInput
	if err := json.Unmarshal(input, &req); err != nil {
		var pathString string
		if stringErr := json.Unmarshal(input, &pathString); stringErr != nil {
			return "", errors.New("invalid read_file input")
		}
		req.Path = pathString
	}
	if strings.TrimSpace(req.Path) == "" {
		pathValue, err := extractPathAlias(input, "path", "file", "filepath")
		if err != nil {
			return "", errors.New("invalid read_file input")
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

	text, err := ExtractSummarizableText(absPath, t.allowedDirs)
	if err != nil {
		if strings.EqualFold(filepath.Ext(absPath), ".pdf") {
			return "", fmt.Errorf("read pdf: %w", err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "binary files are not supported") {
			return "", errors.New("binary files are not supported")
		}
		return "", fmt.Errorf("read file: %w", err)
	}
	if len(text) > 16000 {
		text = text[:16000] + "\n...[truncated]"
	}
	return fmt.Sprintf("Contents of %s:\n%s", absPath, text), nil
}

func isTextLike(data []byte) bool {
	const sampleLimit = 4096
	limit := len(data)
	if limit > sampleLimit {
		limit = sampleLimit
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return false
		}
	}
	return true
}
