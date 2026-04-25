package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileToolReadsTextFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "todo.md")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write todo.md: %v", err)
	}

	tool := NewReadFileTool([]string{root})
	input, _ := json.Marshal(map[string]string{"path": path})
	result, err := tool.Execute(input)
	if err != nil {
		t.Fatalf("execute read_file: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Fatalf("expected file content, got: %s", result)
	}
}
