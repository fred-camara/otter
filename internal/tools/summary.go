package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type SummarizeFilesTool struct {
	allowedDirs []string
}

type summarizeFilesInput struct {
	Paths []string `json:"paths"`
}

func NewSummarizeFilesTool(allowedDirs []string) *SummarizeFilesTool {
	return &SummarizeFilesTool{allowedDirs: allowedDirs}
}

func (t *SummarizeFilesTool) Name() string {
	return "summarize_files"
}

func (t *SummarizeFilesTool) Description() string {
	return "Summarizes text files by extracting first lines and key counts."
}

func (t *SummarizeFilesTool) Execute(input json.RawMessage) (string, error) {
	var req summarizeFilesInput
	if err := json.Unmarshal(input, &req); err != nil {
		return "", errors.New("invalid summarize_files input")
	}
	if len(req.Paths) == 0 {
		paths, err := extractPathsAlias(input, "paths", "files")
		if err != nil {
			return "", errors.New("invalid summarize_files input")
		}
		req.Paths = paths
	}
	if len(req.Paths) == 0 {
		return "", errors.New("paths is required")
	}

	lines := make([]string, 0, len(req.Paths))
	for _, rawPath := range req.Paths {
		pathValue := strings.TrimSpace(rawPath)
		if pathValue == "" {
			continue
		}

		absPath, err := ResolvePath(pathValue)
		if err != nil {
			return "", fmt.Errorf("resolve path %q: %w", pathValue, err)
		}
		if !isPathAllowed(absPath, t.allowedDirs) {
			return "", fmt.Errorf("path is outside allowed directories: %s", absPath)
		}
		if isHiddenPath(absPath) {
			return "", fmt.Errorf("hidden paths are not allowed: %s", absPath)
		}

		content, err := os.ReadFile(absPath)
		if err != nil {
			return "", fmt.Errorf("read file %s: %w", absPath, err)
		}
		if !isTextLike(content) {
			return "", fmt.Errorf("binary files are not supported: %s", absPath)
		}

		text := strings.TrimSpace(string(content))
		summaryLine := summarizeText(filepath.Base(absPath), text)
		lines = append(lines, summaryLine)
	}

	if len(lines) == 0 {
		return "", errors.New("no valid paths were provided")
	}

	return "Summary:\n- " + strings.Join(lines, "\n- "), nil
}

func summarizeText(name, text string) string {
	if text == "" {
		return fmt.Sprintf("%s: empty file", name)
	}
	lineCount := 1 + strings.Count(text, "\n")
	wordCount := len(strings.Fields(text))
	firstLine := text
	if idx := strings.IndexRune(text, '\n'); idx >= 0 {
		firstLine = text[:idx]
	}
	if len(firstLine) > 120 {
		firstLine = firstLine[:120] + "..."
	}
	return fmt.Sprintf("%s: %d lines, %d words, starts with %q", name, lineCount, wordCount, firstLine)
}

func extractPathsAlias(raw json.RawMessage, aliases ...string) ([]string, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	for _, alias := range aliases {
		value, ok := payload[alias]
		if !ok {
			continue
		}
		items, ok := value.([]any)
		if !ok {
			continue
		}
		paths := make([]string, 0, len(items))
		for _, item := range items {
			asText, ok := item.(string)
			if !ok {
				continue
			}
			asText = strings.TrimSpace(asText)
			if asText != "" {
				paths = append(paths, asText)
			}
		}
		if len(paths) > 0 {
			return paths, nil
		}
	}
	return nil, errors.New("missing paths")
}
