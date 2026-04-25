package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"otter/internal/audit"
	"otter/internal/planner"
	"otter/internal/recovery"
)

type stubPlanner struct {
	output string
	err    error
}

type stubModelGenerator struct {
	output string
	err    error
}

func (s stubPlanner) Plan(_ context.Context, _ planner.Request) (planner.Response, error) {
	if s.err != nil {
		return planner.Response{}, s.err
	}
	return planner.Response{RawJSON: s.output}, nil
}

func (s stubModelGenerator) Generate(_ string) (string, error) {
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

func TestDirectToolCallForSummarizeThisFile(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("summarize this file: ~/Downloads/CV_Frederic_Camara_2025.pdf")
	if !ok {
		t.Fatalf("expected direct summarize tool call")
	}
	if call.Tool != "summarize_files" {
		t.Fatalf("expected summarize_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), "CV_Frederic_Camara_2025.pdf") {
		t.Fatalf("expected file path in input, got %s", string(call.Input))
	}
}

func TestDirectToolCallForSummarizeThisFileWithoutColon(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("summarize this file CV_Frederic_Camara_2025.pdf")
	if !ok {
		t.Fatalf("expected direct summarize tool call")
	}
	if call.Tool != "summarize_files" {
		t.Fatalf("expected summarize_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), "CV_Frederic_Camara_2025.pdf") {
		t.Fatalf("expected file path in input, got %s", string(call.Input))
	}
	if strings.Contains(string(call.Input), "this file") {
		t.Fatalf("should strip conversational prefix, got %s", string(call.Input))
	}
}

func TestRunSummarizeThisFilePdfReturnsToolErrorInsteadOfPlannerJSONError(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "CV_Frederic_Camara_2025.pdf")
	if err := os.WriteFile(pdfPath, []byte{0x25, 0x50, 0x44, 0x46, 0x00}, 0o644); err != nil {
		t.Fatalf("seed pdf: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("summarize this file: " + pdfPath)
	if strings.Contains(strings.ToLower(result), "planner returned invalid json") {
		t.Fatalf("expected direct tool path, got planner JSON error: %s", result)
	}
	lower := strings.ToLower(result)
	if !strings.Contains(lower, "read pdf") && !strings.Contains(lower, "binary files are not supported") {
		t.Fatalf("expected direct summarize tool error for pdf input, got: %s", result)
	}
}

func TestSummarizeThisFileResolvesBareFilenameInAllowedDirs(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "docs", "cv")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(deep, "cv.txt")
	if err := os.WriteFile(file, []byte("hello\nworld"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("summarize this file: cv.txt")
	if strings.Contains(strings.ToLower(result), "planner returned invalid json") {
		t.Fatalf("should not hit planner json path: %s", result)
	}
	if !strings.Contains(result, "Summary:") || !strings.Contains(result, "cv.txt") {
		t.Fatalf("expected summary output for resolved file, got: %s", result)
	}
}

func TestRunSummarizeUsesModelOutputWhenAvailable(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "cv.txt")
	if err := os.WriteFile(file, []byte("Frederic Camara\nStaff engineer"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	orch.modelGen = stubModelGenerator{output: "Model summary output"}

	result := orch.Run("summarize this file: cv.txt")
	if !strings.Contains(result, "Model summary output") {
		t.Fatalf("expected model output, got: %s", result)
	}
}

func TestRunSummarizeShowsExplicitFallbackWhenModelFails(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "cv.txt")
	if err := os.WriteFile(file, []byte("Frederic Camara\nStaff engineer"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	orch.modelGen = stubModelGenerator{err: fmt.Errorf("ollama unavailable")}

	result := orch.Run("summarize this file: cv.txt")
	if !strings.Contains(result, "Model summary unavailable: ollama unavailable") {
		t.Fatalf("expected explicit model fallback reason, got: %s", result)
	}
	if !strings.Contains(result, "Using tool-based fallback.") {
		t.Fatalf("expected fallback marker, got: %s", result)
	}
	if !strings.Contains(result, "Summary:") {
		t.Fatalf("expected fallback summary body, got: %s", result)
	}
}

func TestSummarizeThisFileErrorsOnAmbiguousBareFilename(t *testing.T) {
	root := t.TempDir()
	firstDir := filepath.Join(root, "a")
	secondDir := filepath.Join(root, "b")
	if err := os.MkdirAll(firstDir, 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.MkdirAll(secondDir, 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(firstDir, "duplicate.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a duplicate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "duplicate.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b duplicate: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("summarize this file: duplicate.txt")
	if !strings.Contains(strings.ToLower(result), "multiple files named") {
		t.Fatalf("expected ambiguity message, got: %s", result)
	}
}

func TestRunCreatesAuditDirectoryAndFinalOutput(t *testing.T) {
	t.Setenv("OTTER_AUDIT_DISABLED", "false")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	t.Setenv("OTTER_AUDIT_RUNS_DIR", filepath.Join(home, ".config", "otter", "runs"))

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("list files in "+root, "cli")
	if !strings.Contains(result, "Visible entries") {
		t.Fatalf("unexpected run result: %s", result)
	}

	runDir, err := audit.ResolveRunDirectory("latest")
	if err != nil {
		t.Fatalf("resolve latest run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "final_output.md")); err != nil {
		t.Fatalf("expected final_output.md: %v", err)
	}
	meta, err := os.ReadFile(filepath.Join(runDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if !strings.Contains(string(meta), `"status": "success"`) {
		t.Fatalf("expected success status in metadata, got %s", string(meta))
	}
}

func TestRunLogsInvalidPlannerJSONRawAndError(t *testing.T) {
	t.Setenv("OTTER_AUDIT_DISABLED", "false")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	t.Setenv("OTTER_AUDIT_RUNS_DIR", filepath.Join(home, ".config", "otter", "runs"))

	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: "definitely-not-json"})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("plan my day", "cli")
	if !strings.Contains(strings.ToLower(result), "invalid json") {
		t.Fatalf("expected invalid json result, got %s", result)
	}
	runDir, err := audit.ResolveRunDirectory("latest")
	if err != nil {
		t.Fatalf("resolve latest run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "planner_response_raw.txt"))
	if err != nil || !strings.Contains(string(raw), "definitely-not-json") {
		t.Fatalf("expected raw planner response saved, err=%v body=%q", err, string(raw))
	}
	errs, err := os.ReadFile(filepath.Join(runDir, "errors.jsonl"))
	if err != nil || !strings.Contains(string(errs), "planner_parse") {
		t.Fatalf("expected parse error logged, err=%v body=%q", err, string(errs))
	}
	finalOutput, err := os.ReadFile(filepath.Join(runDir, "final_output.md"))
	if err != nil || !strings.Contains(strings.ToLower(string(finalOutput)), "invalid json") {
		t.Fatalf("expected final output persisted, err=%v body=%q", err, string(finalOutput))
	}
	meta, err := os.ReadFile(filepath.Join(runDir, "metadata.json"))
	if err != nil || !strings.Contains(string(meta), `"status": "failure"`) {
		t.Fatalf("expected failure status in metadata, err=%v body=%q", err, string(meta))
	}
}

func TestRunLogsToolCalls(t *testing.T) {
	t.Setenv("OTTER_AUDIT_DISABLED", "false")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	t.Setenv("OTTER_AUDIT_RUNS_DIR", filepath.Join(home, ".config", "otter", "runs"))

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	_ = orch.RunWithMode("list files in "+root, "cli")
	runDir, err := audit.ResolveRunDirectory("latest")
	if err != nil {
		t.Fatalf("resolve latest run: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(runDir, "tool_calls.jsonl"))
	if err != nil || !strings.Contains(string(calls), "list_files") {
		t.Fatalf("expected tool call log, err=%v body=%q", err, string(calls))
	}
}

func TestAuditFailureDoesNotBreakExecution(t *testing.T) {
	t.Setenv("OTTER_CONFIG_FILE", "/dev/null/config.json")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	result := orch.RunWithMode("list files in "+root, "cli")
	if !strings.Contains(result, "Visible entries") {
		t.Fatalf("execution should still succeed without audit writes, got %s", result)
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

func TestRunReadThenWriteCreatesNewFile(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "jamma-yoon.md")
	second := filepath.Join(root, "prospects.md")
	output := filepath.Join(root, "outreach-plan.md")

	if err := os.WriteFile(first, []byte("Jamma: founder of Example"), 0o644); err != nil {
		t.Fatalf("write first source: %v", err)
	}
	if err := os.WriteFile(second, []byte("Prospects: focus on design agencies"), 0o644); err != nil {
		t.Fatalf("write second source: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	task := fmt.Sprintf("read %s and %s, then write a new outreach plan to %s", first, second, output)
	result := orch.Run(task)
	if !strings.Contains(result, output) {
		t.Fatalf("expected output path in response, got: %s", result)
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !strings.Contains(string(data), "Otter Synthesized Plan") {
		t.Fatalf("expected synthesized report content, got: %s", string(data))
	}
}

func TestRunOrganizeDownloadsMovesFiles(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "a.pdf"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed download file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "b.png"), []byte("y"), 0o644); err != nil {
		t.Fatalf("seed second download file: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("organize my downloads")
	if !strings.Contains(result, "Moved files") {
		t.Fatalf("expected moved files result, got: %s", result)
	}
	if !strings.Contains(result, "Documents") || !strings.Contains(result, "Images") {
		t.Fatalf("expected categorized folders in result, got: %s", result)
	}
}

func TestRunOrganizeMusicIntoAudioSubfolder(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "song-one.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "song-two.wav"), []byte("y"), 0o644); err != nil {
		t.Fatalf("seed wav: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "paper.pdf"), []byte("z"), 0o644); err != nil {
		t.Fatalf("seed pdf: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("organize my music files and place them into a subfolder called audio")
	if !strings.Contains(result, "Moved files") {
		t.Fatalf("expected moved files result, got: %s", result)
	}
	if !strings.Contains(strings.ToLower(result), "audio") {
		t.Fatalf("expected audio target folder in result, got: %s", result)
	}
	if strings.Contains(result, "paper.pdf") {
		t.Fatalf("non-music files should not be moved, got: %s", result)
	}
}

func TestRunOrganizeMusicConfirmFlagDoesNotBecomeFolder(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("organize my music files and place them into a subfolder called audio confirm=true")
	if strings.Contains(result, "confirm=true") {
		t.Fatalf("confirm flag should not be treated as a folder name: %s", result)
	}
	if !strings.Contains(strings.ToLower(result), "audio") {
		t.Fatalf("expected audio folder in output, got: %s", result)
	}
	if _, err := os.Stat(filepath.Join(downloads, "Audio", "song.mp3")); err != nil {
		t.Fatalf("expected moved file in Audio folder: %v", err)
	}
}

func TestRunRecoveryModeCreatesDryRunPlanFiles(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "audio", "mix")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	source := filepath.Join(nested, "mystery.zzz")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("recover file structure in " + root)
	if !strings.Contains(result, "dry-run only") {
		t.Fatalf("expected dry-run response, got: %s", result)
	}

	jsonPath := filepath.Join(root, "recovery_plan.json")
	mdPath := filepath.Join(root, "recovery_plan.md")
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("expected recovery json to be created: %v", err)
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Fatalf("expected recovery markdown to be created: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source file should remain in place: %v", err)
	}
}

func TestRunRecoveryModeUsesLogSignals(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "organized", "a.wav")
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(current, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	task := fmt.Sprintf("create recovery plan for %s ; %s -> %s", root, filepath.Join(root, "original", "pack", "a.wav"), current)
	_ = orch.Run(task)

	data, err := os.ReadFile(filepath.Join(root, "recovery_plan.json"))
	if err != nil {
		t.Fatalf("read recovery plan json: %v", err)
	}
	var plan recovery.Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("parse recovery plan: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected one plan entry, got %d", len(plan.Entries))
	}
	if plan.Entries[0].Confidence != recovery.ConfidenceHigh {
		t.Fatalf("expected high confidence from logs, got %s", plan.Entries[0].Confidence)
	}
}

func TestRunOrganizeMusicFindsNestedFiles(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	nested := filepath.Join(downloads, "Later")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("organize my music files and place them into a subfolder called audio")
	if !strings.Contains(result, "Moved files") {
		t.Fatalf("expected moved files result, got: %s", result)
	}
	if _, err := os.Stat(filepath.Join(downloads, "Audio", "Later", "song.mp3")); err != nil {
		t.Fatalf("expected moved file in Audio folder: %v", err)
	}
}

func TestRunOrganizeWeirdMusicPromptDefaultsToDownloads(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.Run("organize my music files in audio confirm=true by taking them out and then placing them into a subfolder called audio")
	if strings.Contains(strings.ToLower(result), "no such file") {
		t.Fatalf("source parsing should not create invalid paths: %s", result)
	}
	if !strings.Contains(result, "Moved files") {
		t.Fatalf("expected a move result, got: %s", result)
	}
	if _, err := os.Stat(filepath.Join(downloads, "Audio", "song.mp3")); err != nil {
		t.Fatalf("expected moved file in Audio folder: %v", err)
	}
}

func TestUndoLastMoveRestoresFiles(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	moveResult := orch.Run("organize my music files and place them into a subfolder called audio")
	if !strings.Contains(moveResult, "Moved files") {
		t.Fatalf("expected move result, got: %s", moveResult)
	}
	if _, err := os.Stat(filepath.Join(downloads, "Audio", "song.mp3")); err != nil {
		t.Fatalf("expected moved file in Audio folder: %v", err)
	}

	undoResult := orch.Run("undo last move")
	if !strings.Contains(undoResult, "Undid the last move") {
		t.Fatalf("expected undo result, got: %s", undoResult)
	}
	if _, err := os.Stat(filepath.Join(downloads, "song.mp3")); err != nil {
		t.Fatalf("expected file restored to original location: %v", err)
	}
}
