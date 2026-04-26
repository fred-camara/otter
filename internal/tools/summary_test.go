package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarizeFilesTool(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.md")
	second := filepath.Join(root, "b.md")
	if err := os.WriteFile(first, []byte("first line\nsecond line"), 0o644); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := os.WriteFile(second, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write second: %v", err)
	}

	tool := NewSummarizeFilesTool([]string{root})
	input, _ := json.Marshal(map[string][]string{"paths": []string{first, second}})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("execute summarize_files: %v", err)
	}
	if !strings.Contains(result, "a.md") || !strings.Contains(result, "b.md") {
		t.Fatalf("expected both files in summary, got: %s", result)
	}
}

func TestSummarizeFilesToolSupportsPDF(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "cv.pdf")
	if err := os.WriteFile(pdfPath, buildMinimalPDF("Frederic Camara CV"), 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	tool := NewSummarizeFilesTool([]string{root})
	input, _ := json.Marshal(map[string][]string{"paths": []string{pdfPath}})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("execute summarize_files for pdf: %v", err)
	}
	if !strings.Contains(result, "cv.pdf") {
		t.Fatalf("expected pdf file name in summary, got: %s", result)
	}
	normalized := strings.ReplaceAll(strings.ToLower(result), " ", "")
	if !strings.Contains(normalized, "fredericcamaracv") {
		t.Fatalf("expected extracted pdf text in summary, got: %s", result)
	}
}

func TestSummarizeFilesToolSupportsTableLikePDF(t *testing.T) {
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

	tool := NewSummarizeFilesTool([]string{root})
	input, _ := json.Marshal(map[string][]string{"paths": []string{pdfPath}})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("execute summarize_files for table pdf: %v", err)
	}
	normalized := strings.ReplaceAll(strings.ToLower(result), " ", "")
	if !strings.Contains(normalized, "grosspay") || !strings.Contains(normalized, "1234.56") {
		t.Fatalf("expected gross pay row in summary, got: %s", result)
	}
	if !strings.Contains(normalized, "netpay") || !strings.Contains(normalized, "999.99") {
		t.Fatalf("expected net pay row in summary, got: %s", result)
	}
	if !strings.Contains(normalized, "tax") || !strings.Contains(normalized, "234.57") {
		t.Fatalf("expected tax row in summary, got: %s", result)
	}
}

func buildMinimalPDF(text string) []byte {
	escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(text)
	stream := "BT /F1 18 Tf 50 100 Td (" + escaped + ") Tj ET"

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for index, object := range objects {
		offsets = append(offsets, out.Len())
		out.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", index+1, object))
	}

	xrefOffset := out.Len()
	out.WriteString("xref\n")
	out.WriteString(fmt.Sprintf("0 %d\n", len(objects)+1))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		out.WriteString(fmt.Sprintf("%010d 00000 n \n", offset))
	}
	out.WriteString("trailer\n")
	out.WriteString(fmt.Sprintf("<< /Size %d /Root 1 0 R >>\n", len(objects)+1))
	out.WriteString("startxref\n")
	out.WriteString(fmt.Sprintf("%d\n", xrefOffset))
	out.WriteString("%%EOF\n")
	return out.Bytes()
}

func buildTablePDF(rows []string) []byte {
	var stream strings.Builder
	stream.WriteString("BT /F1 12 Tf 40 120 Td\n")
	for index, row := range rows {
		if index > 0 {
			stream.WriteString("T*\n")
		}
		escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(row)
		stream.WriteString("(")
		stream.WriteString(escaped)
		stream.WriteString(") Tj\n")
	}
	stream.WriteString("ET")

	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 400 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", stream.Len(), stream.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, 0, len(objects))
	for index, object := range objects {
		offsets = append(offsets, out.Len())
		out.WriteString(fmt.Sprintf("%d 0 obj\n%s\nendobj\n", index+1, object))
	}

	xrefOffset := out.Len()
	out.WriteString("xref\n")
	out.WriteString(fmt.Sprintf("0 %d\n", len(objects)+1))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		out.WriteString(fmt.Sprintf("%010d 00000 n \n", offset))
	}
	out.WriteString("trailer\n")
	out.WriteString(fmt.Sprintf("<< /Size %d /Root 1 0 R >>\n", len(objects)+1))
	out.WriteString("startxref\n")
	out.WriteString(fmt.Sprintf("%d\n", xrefOffset))
	out.WriteString("%%EOF\n")
	return out.Bytes()
}
