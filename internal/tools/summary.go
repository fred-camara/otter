package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pdf "github.com/dslipak/pdf"
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

func ExtractSummarizableText(path string, allowedDirs []string) (string, error) {
	absPath, err := ResolvePath(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if !isPathAllowed(absPath, allowedDirs) {
		return "", fmt.Errorf("path is outside allowed directories: %s", absPath)
	}
	if isHiddenPath(absPath) {
		return "", fmt.Errorf("hidden paths are not allowed: %s", absPath)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", absPath, err)
	}

	var text string
	if strings.EqualFold(filepath.Ext(absPath), ".pdf") {
		text, err = extractPDFText(absPath)
		if err != nil {
			return "", fmt.Errorf("read pdf %s: %w", absPath, err)
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("pdf has no extractable text: %s", absPath)
		}
	} else {
		if !isTextLike(content) {
			return "", fmt.Errorf("binary files are not supported: %s", absPath)
		}
		text = string(content)
	}

	text = sanitizeExtractedText(text)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("file has no usable text content: %s", absPath)
	}
	if len(text) > 64000 {
		text = text[:64000]
	}
	return strings.TrimSpace(text), nil
}

func extractPDFText(path string) (string, error) {
	reader, err := pdf.Open(path)
	if err != nil {
		return "", err
	}

	rowText, err := extractPDFTextByRows(reader)
	if err != nil {
		return "", err
	}

	plainTextReader, err := reader.GetPlainText()
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(plainTextReader, 4<<20))
	if err != nil {
		return "", err
	}

	plainText := strings.TrimSpace(string(raw))
	if len(plainText) > 64000 {
		plainText = plainText[:64000]
	}

	if preferStructuredPDFText(rowText, plainText) {
		return rowText, nil
	}
	return plainText, nil
}

func extractPDFTextByRows(reader *pdf.Reader) (string, error) {
	pages := reader.NumPage()
	if pages == 0 {
		return "", nil
	}

	lines := make([]string, 0, 64)
	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		rows, err := page.GetTextByRow()
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			parts := make([]string, 0, len(row.Content))
			for _, item := range row.Content {
				part := strings.TrimSpace(item.S)
				if part != "" {
					parts = append(parts, part)
				}
			}
			if len(parts) == 0 {
				continue
			}
			line := strings.Join(parts, " ")
			line = strings.Join(strings.Fields(line), " ")
			if line != "" {
				lines = append(lines, line)
			}
		}
	}

	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(text) > 64000 {
		text = text[:64000]
	}
	return text, nil
}

func preferStructuredPDFText(rowText, plainText string) bool {
	rowText = strings.TrimSpace(rowText)
	plainText = strings.TrimSpace(plainText)
	if rowText == "" {
		return false
	}
	if plainText == "" {
		return true
	}
	rowLines := 1 + strings.Count(rowText, "\n")
	plainLines := 1 + strings.Count(plainText, "\n")
	if rowLines > 1 && rowLines >= plainLines/2 {
		return true
	}
	if len(rowText) >= int(float64(len(plainText))*0.75) {
		return true
	}
	return false
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
