package tools

import (
	"encoding/json"
	"errors"
	"fmt"
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
		var singlePath string
		if stringErr := json.Unmarshal(input, &singlePath); stringErr != nil {
			return "", errors.New("invalid summarize_files input")
		}
		req.Paths = []string{singlePath}
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
		text, err := ExtractSummarizableText(absPath, t.allowedDirs)
		if err != nil {
			return "", err
		}
		summaryLine := summarizeText(filepath.Base(absPath), text)
		lines = append(lines, summaryLine)
	}

	if len(lines) == 0 {
		return "", errors.New("no valid paths were provided")
	}

	return "Summary:\n- " + strings.Join(lines, "\n- "), nil
}

func sanitizeExtractedText(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	builder := strings.Builder{}
	builder.Grow(len(text))
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 32 && r != 127) {
			builder.WriteRune(r)
			continue
		}
		builder.WriteRune(' ')
	}
	clean := strings.ReplaceAll(builder.String(), "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	return clean
}

func summarizeText(name, text string) string {
	if text == "" {
		return fmt.Sprintf("%s: empty file", name)
	}
	lineCount := 1 + strings.Count(text, "\n")
	wordCount := len(strings.Fields(text))
	lines := strings.Split(text, "\n")
	previewLines := make([]string, 0, 3)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Trim(line, "|- :") == "" {
			continue
		}
		previewLines = append(previewLines, line)
		if len(previewLines) == 3 {
			break
		}
	}
	preview := text
	if len(previewLines) > 0 {
		preview = strings.Join(previewLines, " | ")
	}
	if len(preview) > 120 {
		preview = preview[:120] + "..."
	}
	return fmt.Sprintf("%s: %d lines, %d words, starts with %q", name, lineCount, wordCount, preview)
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
