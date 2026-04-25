package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileToolRejectsOutsideAllowedDir(t *testing.T) {
	allowed := t.TempDir()
	outside := filepath.Join(t.TempDir(), "note.md")
	tool := NewWriteFileTool([]string{allowed})

	input, _ := json.Marshal(map[string]any{
		"path":    outside,
		"content": "hello",
	})
	_, err := tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "outside allowed") {
		t.Fatalf("expected outside-allowed error, got %v", err)
	}
}

func TestWriteFileToolRejectsInvalidPath(t *testing.T) {
	tool := NewWriteFileTool([]string{t.TempDir()})
	input, _ := json.Marshal(map[string]any{
		"content": "hello",
	})
	_, err := tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("expected required-path error, got %v", err)
	}
}

func TestWriteFileToolBlocksOverwriteWithoutConfirm(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "report.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	tool := NewWriteFileTool([]string{root})
	input, _ := json.Marshal(map[string]any{
		"path":      path,
		"content":   "new",
		"overwrite": true,
	})
	_, err := tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "confirm=true") {
		t.Fatalf("expected confirm error, got %v", err)
	}
}

func TestWriteFileToolWritesNewFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "reports", "new.md")
	tool := NewWriteFileTool([]string{root})

	input, _ := json.Marshal(map[string]any{
		"path":    path,
		"content": "hello world",
	})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("write file failed: %v", err)
	}
	if !strings.Contains(result, path) {
		t.Fatalf("expected output path in result, got %q", result)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("unexpected file content %q", string(got))
	}
}

func TestMoveFileToolRejectsOutsideAllowedDir(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	source := filepath.Join(outside, "a.txt")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	target := filepath.Join(outside, "b.txt")

	tool := NewMoveFileTool([]string{allowed})
	input, _ := json.Marshal(map[string]any{
		"source": source,
		"target": target,
	})
	_, err := tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "within allowed") {
		t.Fatalf("expected outside-allowed error, got %v", err)
	}
}

func TestMoveFileToolRejectsExistingTarget(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "a.txt")
	target := filepath.Join(root, "b.txt")
	if err := os.WriteFile(source, []byte("a"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if err := os.WriteFile(target, []byte("b"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	tool := NewMoveFileTool([]string{root})
	input, _ := json.Marshal(map[string]any{
		"source": source,
		"target": target,
	})
	_, err := tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "target already exists") {
		t.Fatalf("expected target exists error, got %v", err)
	}
}

func TestMoveFileToolDryRunForBatch(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.txt")
	second := filepath.Join(root, "b.txt")
	if err := os.WriteFile(first, []byte("a"), 0o644); err != nil {
		t.Fatalf("seed first: %v", err)
	}
	if err := os.WriteFile(second, []byte("b"), 0o644); err != nil {
		t.Fatalf("seed second: %v", err)
	}

	tool := NewMoveFileTool([]string{root})
	input, _ := json.Marshal(map[string]any{
		"moves": []map[string]string{
			{"source": first, "target": filepath.Join(root, "docs", "a.txt")},
			{"source": second, "target": filepath.Join(root, "docs", "b.txt")},
		},
	})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if !strings.Contains(result, "Dry-run move plan:") {
		t.Fatalf("expected dry-run result, got %q", result)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("source should remain in place on dry-run: %v", err)
	}
}

func TestMoveFileToolMovesSingleFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "a.txt")
	target := filepath.Join(root, "docs", "a.txt")
	if err := os.WriteFile(source, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	tool := NewMoveFileTool([]string{root})
	input, _ := json.Marshal(map[string]any{
		"source": source,
		"target": target,
	})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("move failed: %v", err)
	}
	if !strings.Contains(result, target) {
		t.Fatalf("expected moved target in output, got %q", result)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source should not exist after move, stat err=%v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target should exist after move: %v", err)
	}
}
