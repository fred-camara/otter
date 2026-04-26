package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type PageKind string

const (
	PageKindTextNative PageKind = "text_native"
	PageKindTableLike  PageKind = "table_like"
	PageKindImageOnly  PageKind = "image_only"
	PageKindMixed      PageKind = "mixed"
	PageKindEmpty      PageKind = "empty"
	PageKindFailed     PageKind = "failed"
)

type DocumentMetadata struct {
	FileSizeBytes int64
	PageCount     int
	ModifiedAt    time.Time
	NativePages   int
	TablePages    int
	ImagePages    int
	MixedPages    int
	OCRPages      int
	FailedPages   int
}

type ExtractionWarning struct {
	Code       string
	Message    string
	PageNumber int
}

type TextBlock struct {
	Text     string
	X        float64
	Y        float64
	Width    float64
	Height   float64
	Font     string
	FontSize float64
}

type ExtractedTable struct {
	Rows       [][]string
	Markdown   string
	Confidence float64
}

type ExtractedImage struct {
	Index      int
	Width      int
	Height     int
	Caption    string
	OCRText    string
	AltSummary string
}

type ExtractedPage struct {
	PageNumber     int
	Kind           PageKind
	Method         string
	Text           string
	Blocks         []TextBlock
	Tables         []ExtractedTable
	Images         []ExtractedImage
	OCRText        string
	CharCount      int
	Confidence     float64
	FallbackReason string
	Warnings       []ExtractionWarning
}

type DocumentChunk struct {
	ID            string
	PageStart     int
	PageEnd       int
	Text          string
	Kind          string
	TokenEstimate int
	Metadata      map[string]string
}

type ExtractedDocument struct {
	SourcePath string
	FileType   string
	Metadata   DocumentMetadata
	Pages      []ExtractedPage
	Chunks     []DocumentChunk
	Warnings   []ExtractionWarning
}

type OCRPageRequest struct {
	SourcePath string
	PageNumber int
	PageKind   PageKind
}

type OCRResult struct {
	Text       string
	Confidence float64
	Warnings   []ExtractionWarning
}

type OCRProvider interface {
	Name() string
	OCRPage(ctx context.Context, req OCRPageRequest) (OCRResult, error)
}

type NoopOCRProvider struct{}

func (NoopOCRProvider) Name() string {
	return "noop"
}

func (NoopOCRProvider) OCRPage(_ context.Context, req OCRPageRequest) (OCRResult, error) {
	return OCRResult{}, fmt.Errorf("ocr unavailable for page %d", req.PageNumber)
}

type ExtractOptions struct {
	MaxPages         int
	MaxFileSizeBytes int64
	PageConcurrency  int
	ChunkTargetChars int
	OCR              OCRProvider
}

func defaultExtractOptions() ExtractOptions {
	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	return ExtractOptions{
		MaxPages:         400,
		MaxFileSizeBytes: 64 << 20,
		PageConcurrency:  workers,
		ChunkTargetChars: 3200,
		OCR:              NoopOCRProvider{},
	}
}

func normalizeExtractOptions(opts ExtractOptions) ExtractOptions {
	defaults := defaultExtractOptions()
	if opts.MaxPages <= 0 {
		opts.MaxPages = defaults.MaxPages
	}
	if opts.MaxFileSizeBytes <= 0 {
		opts.MaxFileSizeBytes = defaults.MaxFileSizeBytes
	}
	if opts.PageConcurrency <= 0 {
		opts.PageConcurrency = defaults.PageConcurrency
	}
	if opts.PageConcurrency > 4 {
		opts.PageConcurrency = 4
	}
	if opts.ChunkTargetChars <= 0 {
		opts.ChunkTargetChars = defaults.ChunkTargetChars
	}
	if opts.OCR == nil {
		opts.OCR = defaults.OCR
	}
	return opts
}

func ExtractDocument(path string, allowedDirs []string) (*ExtractedDocument, error) {
	return ExtractDocumentWithOptions(context.Background(), path, allowedDirs, ExtractOptions{})
}

func ExtractDocumentWithOptions(ctx context.Context, path string, allowedDirs []string, opts ExtractOptions) (*ExtractedDocument, error) {
	absPath, info, err := validateExtractPath(path, allowedDirs)
	if err != nil {
		return nil, err
	}
	opts = normalizeExtractOptions(opts)

	if strings.EqualFold(filepath.Ext(absPath), ".pdf") {
		extractor := NewPDFExtractor(opts)
		return extractor.Extract(ctx, absPath, info)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", absPath, err)
	}
	if !isTextLike(content) {
		return nil, fmt.Errorf("binary files are not supported: %s", absPath)
	}
	text := sanitizeExtractedText(string(content))
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("file has no usable text content: %s", absPath)
	}
	if len(text) > 64000 {
		text = text[:64000]
	}
	chunk := DocumentChunk{
		ID:            "chunk-1",
		PageStart:     1,
		PageEnd:       1,
		Text:          strings.TrimSpace(text),
		Kind:          string(PageKindTextNative),
		TokenEstimate: estimateTokenCount(text),
		Metadata: map[string]string{
			"source_path": absPath,
		},
	}
	return &ExtractedDocument{
		SourcePath: absPath,
		FileType:   strings.TrimPrefix(strings.ToLower(filepath.Ext(absPath)), "."),
		Metadata: DocumentMetadata{
			FileSizeBytes: info.Size(),
			PageCount:     1,
			ModifiedAt:    info.ModTime(),
			NativePages:   1,
		},
		Pages: []ExtractedPage{
			{
				PageNumber: 1,
				Kind:       PageKindTextNative,
				Text:       strings.TrimSpace(text),
				Confidence: 1,
			},
		},
		Chunks: []DocumentChunk{chunk},
	}, nil
}

func ExtractSummarizableText(path string, allowedDirs []string) (string, error) {
	doc, err := ExtractDocument(path, allowedDirs)
	if err != nil {
		if strings.EqualFold(filepath.Ext(path), ".pdf") || strings.EqualFold(filepath.Ext(path), ".PDF") {
			resolvedPath := path
			if absPath, resolveErr := ResolvePath(path); resolveErr == nil {
				resolvedPath = absPath
			}
			return "", fmt.Errorf("read pdf %s: %w", resolvedPath, err)
		}
		return "", err
	}
	text := FlattenDocumentText(doc)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("file has no usable text content: %s", doc.SourcePath)
	}
	if len(text) > 64000 {
		text = text[:64000]
	}
	return strings.TrimSpace(text), nil
}

func FlattenDocumentText(doc *ExtractedDocument) string {
	if doc == nil {
		return ""
	}
	parts := make([]string, 0, len(doc.Pages))
	for _, page := range doc.Pages {
		text := strings.TrimSpace(page.Text)
		if text == "" {
			text = strings.TrimSpace(page.OCRText)
		}
		if text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 && len(doc.Chunks) > 0 {
		for _, chunk := range doc.Chunks {
			trimmed := strings.TrimSpace(chunk.Text)
			if trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}
	return sanitizeExtractedText(strings.Join(parts, "\n\n"))
}

func validateExtractPath(path string, allowedDirs []string) (string, os.FileInfo, error) {
	absPath, err := ResolvePath(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve path %q: %w", path, err)
	}
	if !isPathAllowed(absPath, allowedDirs) {
		return "", nil, fmt.Errorf("path is outside allowed directories: %s", absPath)
	}
	if isHiddenPath(absPath) {
		return "", nil, fmt.Errorf("hidden paths are not allowed: %s", absPath)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", nil, fmt.Errorf("stat file %s: %w", absPath, err)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("path is a directory: %s", absPath)
	}
	return absPath, info, nil
}

func estimateTokenCount(text string) int {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	return (len(trimmed) + 3) / 4
}
