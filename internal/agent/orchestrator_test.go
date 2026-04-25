package agent

import (
	"encoding/json"
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
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("list files in ~/Downloads")
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

func TestDirectToolCallForLatestNotes(t *testing.T) {
	root := t.TempDir()
	notesDir := filepath.Join(root, "notes")
	if err := os.MkdirAll(notesDir, 0o755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}

	recentPath := filepath.Join(notesDir, "recent.md")
	if err := os.WriteFile(recentPath, []byte("recent note"), 0o644); err != nil {
		t.Fatalf("write recent.md: %v", err)
	}

	orch := &Orchestrator{allowedDirs: []string{notesDir}}
	call, ok := orch.directToolCallForTask("Read my latest notes over the last 10 days")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if call.Tool != "summarize_files" {
		t.Fatalf("expected summarize_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), "recent.md") {
		t.Fatalf("expected recent note in tool input, got %s", string(call.Input))
	}
}

func TestDirectToolCallForAccessAddDesktop(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	t.Setenv("HOME", home)

	orch := &Orchestrator{allowedDirs: []string{}}
	call, ok := orch.directToolCallForTask("I'd like otter to have access to Desktop")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if !strings.Contains(call.Error, "Added directory access") {
		t.Fatalf("expected access-added message, got %q", call.Error)
	}
	if !strings.Contains(call.Error, desktop) {
		t.Fatalf("expected desktop path in message, got %q", call.Error)
	}
}

func TestDirectToolCallForAccessList(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"/tmp/a", "/tmp/b"}}
	call, ok := orch.directToolCallForTask("what directories can otter access?")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if !strings.Contains(call.Error, "/tmp/a") || !strings.Contains(call.Error, "/tmp/b") {
		t.Fatalf("expected both directories in access list, got %q", call.Error)
	}
}

func TestInferSafeToolFromTask(t *testing.T) {
	if got := inferSafeToolFromTask("show files in my downloads"); got != "list_files" {
		t.Fatalf("expected list_files, got %q", got)
	}
	if got := inferSafeToolFromTask("open this file"); got != "read_file" {
		t.Fatalf("expected read_file, got %q", got)
	}
	if got := inferSafeToolFromTask("summarize these notes"); got != "summarize_files" {
		t.Fatalf("expected summarize_files, got %q", got)
	}
}

func TestExtractAccessTargetsNotesIncludesCommonPaths(t *testing.T) {
	targets, err := extractAccessTargets("access my notes")
	if err != nil {
		t.Fatalf("extractAccessTargets error: %v", err)
	}
	joined := strings.Join(targets, "|")
	if !strings.Contains(joined, "~/notes") {
		t.Fatalf("expected ~/notes target, got %v", targets)
	}
	if !strings.Contains(joined, "~/Documents/Notes") {
		t.Fatalf("expected ~/Documents/Notes target, got %v", targets)
	}
}

func TestLooksLikeNoteFile(t *testing.T) {
	if !looksLikeNoteFile("/tmp/daily_notes.md") {
		t.Fatalf("expected notes filename to match")
	}
	if looksLikeNoteFile("/tmp/random.txt") {
		t.Fatalf("did not expect random filename to match")
	}
}

func TestDirectToolCallForHelp(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("help")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if !strings.Contains(call.Error, "I can currently") {
		t.Fatalf("expected help response, got %q", call.Error)
	}
}

func TestExecuteToolCallShowsAccessGuidance(t *testing.T) {
	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	call := ToolCall{
		Tool:  "list_files",
		Input: json.RawMessage(`{"path":"/tmp"}`),
	}
	result := orch.executeToolCall("list files in /tmp", call)
	if !strings.Contains(result, "grant access") {
		t.Fatalf("expected access guidance, got: %s", result)
	}
}
