package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestExtractDocumentBuildsStructuredPDFOutput(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "cv.pdf")
	if err := os.WriteFile(pdfPath, buildMinimalPDF("Frederic Camara CV"), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	doc, err := ExtractDocument(pdfPath, []string{root})
	if err != nil {
		t.Fatalf("extract document: %v", err)
	}
	if doc.FileType != "pdf" {
		t.Fatalf("expected pdf file type, got %q", doc.FileType)
	}
	if len(doc.Pages) != 1 {
		t.Fatalf("expected one page, got %d", len(doc.Pages))
	}
	if doc.Pages[0].Kind != PageKindTextNative {
		t.Fatalf("expected text native page, got %q", doc.Pages[0].Kind)
	}
	if !strings.Contains(doc.Pages[0].Text, "Frederic Camara CV") {
		t.Fatalf("expected extracted text, got %q", doc.Pages[0].Text)
	}
	if len(doc.Chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(doc.Chunks))
	}
	if doc.Chunks[0].TokenEstimate <= 0 {
		t.Fatalf("expected positive token estimate, got %d", doc.Chunks[0].TokenEstimate)
	}
}

func TestExtractDocumentClassifiesTableLikePDF(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "payslip.pdf")
	if err := os.WriteFile(pdfPath, buildTablePDF([]string{
		"Payslip",
		"Gross Pay 1234.56",
		"Net Pay 999.99",
		"Tax 234.57",
	}), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	doc, err := ExtractDocument(pdfPath, []string{root})
	if err != nil {
		t.Fatalf("extract document: %v", err)
	}
	if doc.Pages[0].Kind != PageKindTableLike {
		t.Fatalf("expected table-like page, got %q", doc.Pages[0].Kind)
	}
	if len(doc.Pages[0].Tables) == 0 {
		t.Fatalf("expected extracted table")
	}
	tableMarkdown := strings.ToLower(doc.Pages[0].Tables[0].Markdown)
	if !strings.Contains(tableMarkdown, "gross pay") || !strings.Contains(tableMarkdown, "1234.56") {
		t.Fatalf("expected markdown table to contain gross pay, got %s", doc.Pages[0].Tables[0].Markdown)
	}
}

func TestBuildDocumentChunksSkipsEmptyFailedPagesAndPreservesOrder(t *testing.T) {
	doc := &ExtractedDocument{
		SourcePath: "/tmp/test.pdf",
		FileType:   "pdf",
		Pages: []ExtractedPage{
			{PageNumber: 1, Kind: PageKindTextNative, Text: "alpha"},
			{PageNumber: 2, Kind: PageKindFailed},
			{PageNumber: 3, Kind: PageKindMixed, Text: "beta"},
		},
	}
	chunks := buildDocumentChunks(doc, 8)
	if len(chunks) != 2 {
		t.Fatalf("expected two chunks, got %d", len(chunks))
	}
	if chunks[0].PageStart != 1 || chunks[0].PageEnd != 1 {
		t.Fatalf("unexpected first chunk pages: %+v", chunks[0])
	}
	if chunks[1].PageStart != 3 || chunks[1].PageEnd != 3 {
		t.Fatalf("unexpected second chunk pages: %+v", chunks[1])
	}
}

func TestRenderPageForChunkStripsLowValueBoilerplate(t *testing.T) {
	page := ExtractedPage{
		PageNumber: 1,
		Kind:       PageKindTableLike,
		Method:     "pdfkit",
		Text: "Ronnie Kritou\nStaff Software Engineer @ Datadog\nPage 1 of 2\n" +
			"Experience\nDatadog",
	}

	rendered := renderPageForChunk(page)
	if strings.Contains(rendered, "Page 1 [table_like]") {
		t.Fatalf("expected page header to be omitted, got %q", rendered)
	}
	if strings.Contains(rendered, "Method: pdfkit") {
		t.Fatalf("expected method line to be omitted, got %q", rendered)
	}
	if strings.Contains(rendered, "Page 1 of 2") {
		t.Fatalf("expected footer line to be omitted, got %q", rendered)
	}
	if !strings.Contains(rendered, "Ronnie Kritou") || !strings.Contains(rendered, "Datadog") {
		t.Fatalf("expected meaningful content to remain, got %q", rendered)
	}
}

func TestRenderPageForChunkSkipsDuplicateTableMarkdown(t *testing.T) {
	page := ExtractedPage{
		PageNumber: 1,
		Kind:       PageKindTableLike,
		Text: strings.Join([]string{
			"Contact",
			"Ronnie Kritou",
			"Datadog",
			"Staff Software Engineer",
		}, "\n"),
		Tables: []ExtractedTable{
			{
				Markdown: strings.Join([]string{
					"| Contact |  |",
					"| --- | --- |",
					"| Ronnie Kritou |  |",
					"| Datadog |  |",
					"| Staff Software Engineer |  |",
				}, "\n"),
			},
		},
	}

	rendered := renderPageForChunk(page)
	if strings.Contains(rendered, "Table:\n") {
		t.Fatalf("expected duplicate table markdown to be omitted, got %q", rendered)
	}
}

func TestRenderPageForChunkKeepsNonDuplicateTableMarkdown(t *testing.T) {
	page := ExtractedPage{
		PageNumber: 1,
		Kind:       PageKindTableLike,
		Text:       "Quarterly results summary",
		Tables: []ExtractedTable{
			{
				Markdown: strings.Join([]string{
					"| Quarter | Revenue |",
					"| --- | --- |",
					"| Q1 2024 | 1200000 |",
				}, "\n"),
			},
		},
	}

	rendered := renderPageForChunk(page)
	if !strings.Contains(rendered, "Table:\n| Quarter | Revenue |") {
		t.Fatalf("expected non-duplicate table markdown to remain, got %q", rendered)
	}
}

func TestExtractDocumentWithOptionsUsesBoundedDefaults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("hello\nworld"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	doc, err := ExtractDocumentWithOptions(context.Background(), path, []string{root}, ExtractOptions{})
	if err != nil {
		t.Fatalf("extract document with options: %v", err)
	}
	if len(doc.Chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(doc.Chunks))
	}
}

func TestChoosePageTextPrefersHigherQualityPDFKitExtraction(t *testing.T) {
	pdfKitText := "Frederic Camara\nSenior Software Engineer\nBerlin, Germany"
	goText := "F r e d e r i c C a m a r a"
	pdfKitScore, _ := scoreExtractedText(pdfKitText)
	goScore, _ := scoreExtractedText(goText)

	text, method, reason := choosePageText(pdfKitText, pdfKitScore, goText, goScore)
	if method != "pdfkit" {
		t.Fatalf("expected pdfkit method, got %q", method)
	}
	if text != pdfKitText {
		t.Fatalf("expected pdfkit text, got %q", text)
	}
	if reason != "" {
		t.Fatalf("expected no fallback reason, got %q", reason)
	}
}

func TestRunWithSuppressedPDFNoiseSuppressesFD2Writes(t *testing.T) {
	originalStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer reader.Close()
	os.Stderr = writer
	defer func() {
		os.Stderr = originalStderr
	}()

	runWithSuppressedPDFNoise(func() {
		_, _ = syscall.Write(2, []byte("interp\t dup\n"))
	})
	_ = writer.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(reader)
	if got := buf.String(); got != "" {
		t.Fatalf("expected no stderr leakage, got %q", got)
	}
}
