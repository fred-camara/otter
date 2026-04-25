package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type stubPlanner struct {
	output string
	err    error
}

func (s stubPlanner) Plan(_ string, _ []string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.output, nil
}

func TestOrchestratorRunsListFilesTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "todo.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write todo.md: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{
		output: fmt.Sprintf(`{"tool":"list_files","input":{"path":%q}}`, root),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("list files in " + root)
	if !strings.Contains(result, "todo.md") {
		t.Fatalf("expected listed file, got: %s", result)
	}
}

func TestOrchestratorPlannerError(t *testing.T) {
	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{err: fmt.Errorf("planner down")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("help me plan tomorrow")
	if !strings.Contains(result, "Planner error") {
		t.Fatalf("expected planner error response, got: %s", result)
	}
}

func TestParseToolCallExtractsEmbeddedJSON(t *testing.T) {
	raw := `I will use a safe tool:
{"tool":"list_files","input":{"path":"."}}`

	call, err := parseToolCall(raw)
	if err != nil {
		t.Fatalf("parseToolCall returned error: %v", err)
	}
	if call.Tool != "list_files" {
		t.Fatalf("expected list_files, got %q", call.Tool)
	}
}

func TestNormalizeToolNameAliases(t *testing.T) {
	if got := normalizeToolName("ls"); got != "list_files" {
		t.Fatalf("expected list_files, got %q", got)
	}
	if got := normalizeToolName("/read"); got != "read_file" {
		t.Fatalf("expected read_file, got %q", got)
	}
	if got := normalizeToolName("summarize"); got != "summarize_files" {
		t.Fatalf("expected summarize_files, got %q", got)
	}
}

func TestDirectToolCallForTaskListFiles(t *testing.T) {
	call, ok := directToolCallForTask("list files in ~/Downloads")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if call.Tool != "list_files" {
		t.Fatalf("expected list_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), `"~/Downloads"`) {
		t.Fatalf("expected path in input, got %s", string(call.Input))
	}
}
