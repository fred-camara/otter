package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListFilesToolListsVisibleEntries(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".secret"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write .secret: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}

	tool, err := NewListFilesTool([]string{root})
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}

	input, _ := json.Marshal(map[string]string{"path": root})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("execute list_files: %v", err)
	}

	if !strings.Contains(result, "- a.txt") || !strings.Contains(result, "- b.txt") || !strings.Contains(result, "- notes/") {
		t.Fatalf("unexpected output: %s", result)
	}
	if strings.Contains(result, ".secret") {
		t.Fatalf("hidden file should be excluded: %s", result)
	}
	if strings.Index(result, "- a.txt") > strings.Index(result, "- b.txt") {
		t.Fatalf("entries should be sorted: %s", result)
	}
}

func TestListFilesToolRejectsOutsideAllowedPath(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()

	tool, err := NewListFilesTool([]string{allowed})
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}

	input, _ := json.Marshal(map[string]string{"path": outside})
	_, err = tool.Execute(input)
	if err == nil || !strings.Contains(err.Error(), "outside allowed") {
		t.Fatalf("expected outside allowed error, got: %v", err)
	}
}
