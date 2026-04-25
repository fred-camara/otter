package tools

import (
	"encoding/json"
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
