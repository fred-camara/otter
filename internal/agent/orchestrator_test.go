package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"otter/internal/audit"
	"otter/internal/planner"
	"otter/internal/recovery"
	"otter/internal/settings"
	"otter/internal/tools"
)

type stubPlanner struct {
	output string
	err    error
}

type stubModelGenerator struct {
	output string
	err    error
}

type blockingModelGenerator struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
}

type trackingModelGenerator struct {
	mu             sync.Mutex
	active         int
	maxActive      int
	prompts        []string
	failOnPages    string
	chunkDelay     time.Duration
	mergeOutput    string
	chunkOutputFmt string
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

func (b *blockingModelGenerator) Generate(_ string) (string, error) {
	if b.started != nil {
		close(b.started)
	}
	if b.release != nil {
		<-b.release
	}
	if b.done != nil {
		close(b.done)
	}
	return "blocked model output", nil
}

func (t *trackingModelGenerator) Generate(prompt string) (string, error) {
	t.mu.Lock()
	t.prompts = append(t.prompts, prompt)
	t.active++
	if t.active > t.maxActive {
		t.maxActive = t.active
	}
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.active--
		t.mu.Unlock()
	}()

	if t.chunkDelay > 0 {
		time.Sleep(t.chunkDelay)
	}
	if strings.Contains(prompt, "Chunk analyses:") {
		if t.mergeOutput != "" {
			return t.mergeOutput, nil
		}
		return "Merged summary", nil
	}
	if t.failOnPages != "" && strings.Contains(prompt, "Pages: "+t.failOnPages) {
		return "", fmt.Errorf("chunk failed")
	}
	if t.chunkOutputFmt != "" {
		match := "unknown"
		if index := strings.Index(prompt, "\nPages: "); index >= 0 {
			rest := prompt[index+8:]
			if newline := strings.Index(rest, "\n"); newline >= 0 {
				match = strings.TrimSpace(rest[:newline])
			}
		}
		return fmt.Sprintf(t.chunkOutputFmt, match), nil
	}
	return "Chunk summary", nil
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

	result := orch.Run("execute quarterly reconciliation")
	if !strings.Contains(result, "Planner error") {
		t.Fatalf("expected planner error response, got: %s", result)
	}
}

func TestConversationalInputBypassesPlanner(t *testing.T) {
	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	result := orch.Run("Hello otter")
	if strings.Contains(strings.ToLower(result), "planner") {
		t.Fatalf("expected conversational response without planner error, got: %s", result)
	}
}

func TestGreetingWithActionableListIntentExecutesTool(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	if err := os.WriteFile(filepath.Join(desktop, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed desktop file: %v", err)
	}
	t.Setenv("HOME", home)

	orch, err := NewOrchestrator([]string{desktop}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("hey can you list the files in desktop", "chat")
	if !strings.Contains(result, "Visible entries in") {
		t.Fatalf("expected list output, got: %s", result)
	}
	if !strings.Contains(result, "note.txt") {
		t.Fatalf("expected desktop file in output, got: %s", result)
	}
}

func TestInvalidPlannerJSONConversationalFallback(t *testing.T) {
	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: "definitely not json"})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	result := orch.Run("how are you?")
	if strings.Contains(strings.ToLower(result), "invalid json") {
		t.Fatalf("expected friendly conversational fallback, got: %s", result)
	}
	if !strings.Contains(strings.ToLower(result), "help") && !strings.Contains(strings.ToLower(result), "local file") {
		t.Fatalf("expected conversational fallback text, got: %s", result)
	}
}

func TestInvalidPlannerJSONToolLikePromptGetsFriendlyNonConversationalFallback(t *testing.T) {
	root := t.TempDir()
	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: "definitely not json"})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	result := orch.Run("summarize notes from today")
	if strings.Contains(strings.ToLower(result), "planner returned invalid json") {
		t.Fatalf("expected friendly fallback, got: %s", result)
	}
	if !strings.Contains(strings.ToLower(result), "couldn't understand") {
		t.Fatalf("expected explicit rephrase guidance, got: %s", result)
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

func TestDirectToolCallForTaskListFilesNaturalLanguageDesktop(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("list the files in desktop")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if call.Tool != "list_files" {
		t.Fatalf("expected list_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), `"~/Desktop"`) {
		t.Fatalf("expected desktop alias to normalize to ~/Desktop, got %s", string(call.Input))
	}
}

func TestDirectToolCallForTaskListFilesVerboseDownloads(t *testing.T) {
	orch := &Orchestrator{allowedDirs: []string{"."}}
	call, ok := orch.directToolCallForTask("cool now list the files in downloads")
	if !ok {
		t.Fatalf("expected direct tool call")
	}
	if call.Tool != "list_files" {
		t.Fatalf("expected list_files, got %q", call.Tool)
	}
	if !strings.Contains(string(call.Input), `"~/Downloads"`) {
		t.Fatalf("expected downloads alias to normalize to ~/Downloads, got %s", string(call.Input))
	}
}

func TestRunComposedListFilesDesktopAndDownloads(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(desktop, "desktop-note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed desktop file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "downloads-note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed downloads file: %v", err)
	}
	t.Setenv("HOME", home)

	orch, err := NewOrchestrator([]string{desktop, downloads}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("Hey can you list the files in Desktop and then list the files in downlaods", "chat")
	if !strings.Contains(result, "Visible entries in "+desktop) {
		t.Fatalf("expected desktop listing, got: %s", result)
	}
	if !strings.Contains(result, "desktop-note.txt") {
		t.Fatalf("expected desktop file in output, got: %s", result)
	}
	if !strings.Contains(result, "Visible entries in "+downloads) {
		t.Fatalf("expected downloads listing, got: %s", result)
	}
	if !strings.Contains(result, "downloads-note.txt") {
		t.Fatalf("expected downloads file in output, got: %s", result)
	}
}

func TestRunComposedListFilesDesktopThenDownloadsShorthand(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(desktop, "desktop-short.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed desktop file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "downloads-short.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed downloads file: %v", err)
	}
	t.Setenv("HOME", home)

	orch, err := NewOrchestrator([]string{desktop, downloads}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("please list the files in desktop and then downloads", "chat")
	if !strings.Contains(result, "Visible entries in "+desktop) {
		t.Fatalf("expected desktop listing, got: %s", result)
	}
	if !strings.Contains(result, "desktop-short.txt") {
		t.Fatalf("expected desktop file in output, got: %s", result)
	}
	if !strings.Contains(result, "Visible entries in "+downloads) {
		t.Fatalf("expected downloads listing, got: %s", result)
	}
	if !strings.Contains(result, "downloads-short.txt") {
		t.Fatalf("expected downloads file in output, got: %s", result)
	}
}

func TestRunComposedListFilesDesktopThenDowloadsTypo(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(desktop, "desktop-typo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed desktop file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "downloads-typo.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed downloads file: %v", err)
	}
	t.Setenv("HOME", home)

	orch, err := NewOrchestrator([]string{desktop, downloads}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("list the files in desktop and then dowloads", "chat")
	if !strings.Contains(result, "Visible entries in "+desktop) {
		t.Fatalf("expected desktop listing, got: %s", result)
	}
	if !strings.Contains(result, "Visible entries in "+downloads) {
		t.Fatalf("expected downloads listing despite typo alias, got: %s", result)
	}
	if !strings.Contains(result, "downloads-typo.txt") {
		t.Fatalf("expected downloads file in output, got: %s", result)
	}
}

func TestRunComposedListFilesDesktopsAndDownloadsPlural(t *testing.T) {
	home := t.TempDir()
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatalf("mkdir desktop: %v", err)
	}
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(desktop, "desktop-plural.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed desktop file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "downloads-plural.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed downloads file: %v", err)
	}
	t.Setenv("HOME", home)

	orch, err := NewOrchestrator([]string{desktop, downloads}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("list the files in desktops and downloads", "chat")
	if !strings.Contains(result, "Visible entries in "+desktop) {
		t.Fatalf("expected desktop listing, got: %s", result)
	}
	if !strings.Contains(result, "Visible entries in "+downloads) {
		t.Fatalf("expected downloads listing, got: %s", result)
	}
	if !strings.Contains(result, "desktop-plural.txt") || !strings.Contains(result, "downloads-plural.txt") {
		t.Fatalf("expected both files in output, got: %s", result)
	}
}

func TestRunComposedReadThenSummarize(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(filePath, []byte("First line\nSecond line"), 0o644); err != nil {
		t.Fatalf("seed note file: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{err: fmt.Errorf("planner should not run")})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	result := orch.RunWithMode("hey can you read "+filePath+" and then summarize it", "chat")
	if !strings.Contains(result, "Summary:") {
		t.Fatalf("expected summarize output, got: %s", result)
	}
	if !strings.Contains(result, "notes.txt") {
		t.Fatalf("expected summarized filename in output, got: %s", result)
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

func TestRunSummarizeTimesOutAndFallsBackToToolSummary(t *testing.T) {
	root := t.TempDir()
	pdfPath := filepath.Join(root, "cv.pdf")
	if err := os.WriteFile(pdfPath, buildMinimalPDF("Frederic Camara CV"), 0o644); err != nil {
		t.Fatalf("seed pdf: %v", err)
	}

	orch, err := NewOrchestrator([]string{root}, stubPlanner{output: `not json`})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	orch.modelGen = &blockingModelGenerator{
		started: started,
		release: release,
		done:    done,
	}
	orch.modelSummaryTimeout = 20 * time.Millisecond

	resultCh := make(chan string, 1)
	go func() {
		resultCh <- orch.Run("summarize this file: " + pdfPath)
	}()

	select {
	case <-started:
	case <-time.After(8 * time.Second):
		t.Fatal("model did not start")
	}

	var result string
	select {
	case result = <-resultCh:
	case <-time.After(12 * time.Second):
		t.Fatal("summarize request hung instead of timing out")
	}
	if !strings.Contains(strings.ToLower(result), "using tool-based fallback") && !strings.Contains(strings.ToLower(result), "summary:") {
		t.Fatalf("expected fallback summary after timeout, got: %s", result)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("blocked model did not unblock after release")
	}
}

func TestSummarizeDocumentsWithModelUsesBoundedConcurrencyAndPreservesMerge(t *testing.T) {
	orch := &Orchestrator{
		modelGen:            &trackingModelGenerator{chunkDelay: 20 * time.Millisecond, mergeOutput: "Merged final summary", chunkOutputFmt: "Chunk %s"},
		modelSummaryTimeout: 2 * time.Second,
		modelSummaryWorkers: 1,
	}
	docs := []*tools.ExtractedDocument{
		{
			SourcePath: "/tmp/a.pdf",
			Chunks: []tools.DocumentChunk{
				{ID: "chunk-1", PageStart: 1, PageEnd: 1, Text: "alpha", Kind: "text_native"},
				{ID: "chunk-2", PageStart: 2, PageEnd: 2, Text: "beta", Kind: "text_native"},
				{ID: "chunk-3", PageStart: 3, PageEnd: 3, Text: "gamma", Kind: "text_native"},
			},
		},
	}

	result, err := orch.summarizeDocumentsWithModel("summarize this", docs)
	if err != nil {
		t.Fatalf("summarize documents: %v", err)
	}
	if !strings.Contains(result, "Merged final summary") {
		t.Fatalf("expected merged output, got: %s", result)
	}
	model := orch.modelGen.(*trackingModelGenerator)
	if model.maxActive > 1 {
		t.Fatalf("expected bounded concurrency of 1, saw %d", model.maxActive)
	}
}

func TestSummarizeDocumentsWithModelRecordsChunkWarnings(t *testing.T) {
	orch := &Orchestrator{
		modelGen:            &trackingModelGenerator{failOnPages: "2-2", mergeOutput: "Merged with warnings"},
		modelSummaryTimeout: 2 * time.Second,
		modelSummaryWorkers: 1,
	}
	docs := []*tools.ExtractedDocument{
		{
			SourcePath: "/tmp/a.pdf",
			Warnings: []tools.ExtractionWarning{
				{Code: "ocr_unavailable", Message: "ocr unavailable", PageNumber: 2},
			},
			Chunks: []tools.DocumentChunk{
				{ID: "chunk-1", PageStart: 1, PageEnd: 1, Text: "alpha", Kind: "text_native"},
				{ID: "chunk-2", PageStart: 2, PageEnd: 2, Text: "beta", Kind: "image_only"},
			},
		},
	}

	result, err := orch.summarizeDocumentsWithModel("summarize this", docs)
	if err != nil {
		t.Fatalf("summarize documents: %v", err)
	}
	if !strings.Contains(result, "Chunk summary") {
		t.Fatalf("expected successful chunk summary, got: %s", result)
	}
	if !strings.Contains(result, "pages 2-2") {
		t.Fatalf("expected chunk failure warning, got: %s", result)
	}
}

func TestSummarizeDocumentsWithModelEmitsProgressStages(t *testing.T) {
	orch := &Orchestrator{
		modelGen:            stubModelGenerator{output: "Chunk summary"},
		modelSummaryTimeout: 2 * time.Second,
		modelSummaryWorkers: 1,
	}
	progress := make([]string, 0, 4)
	orch.SetProgressReporter(func(message string) {
		progress = append(progress, message)
	})

	docs := []*tools.ExtractedDocument{
		{
			SourcePath: "/tmp/profile.pdf",
			Chunks: []tools.DocumentChunk{
				{ID: "chunk-1", PageStart: 1, PageEnd: 1, Text: "alpha", Kind: "text_native"},
				{ID: "chunk-2", PageStart: 2, PageEnd: 2, Text: "beta", Kind: "text_native"},
			},
		},
	}

	_, err := orch.summarizeDocumentsWithModel("summarize this", docs)
	if err != nil {
		t.Fatalf("summarize documents: %v", err)
	}
	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, "Summarizing chunk 1/2") {
		t.Fatalf("expected chunk start progress, got: %s", joined)
	}
	if !strings.Contains(joined, "Completed chunk 1/2") {
		t.Fatalf("expected chunk completion progress, got: %s", joined)
	}
	if !strings.Contains(joined, "Merging 2 chunk summary result(s)") {
		t.Fatalf("expected merge progress, got: %s", joined)
	}
}

func TestSummarizeDocumentsWithModelSkipsMergeForSingleSuccessfulChunk(t *testing.T) {
	orch := &Orchestrator{
		modelGen:            stubModelGenerator{output: "## Summary\n\nSingle chunk result"},
		modelSummaryTimeout: 2 * time.Second,
		modelSummaryWorkers: 1,
	}
	progress := make([]string, 0, 2)
	orch.SetProgressReporter(func(message string) {
		progress = append(progress, message)
	})

	docs := []*tools.ExtractedDocument{
		{
			SourcePath: "/tmp/profile.pdf",
			Chunks: []tools.DocumentChunk{
				{ID: "chunk-1", PageStart: 1, PageEnd: 2, Text: "alpha beta", Kind: "text_native"},
			},
		},
	}

	result, err := orch.summarizeDocumentsWithModel("summarize this", docs)
	if err != nil {
		t.Fatalf("summarize documents: %v", err)
	}
	if !strings.Contains(result, "Single chunk result") {
		t.Fatalf("expected direct chunk summary, got: %s", result)
	}
	joined := strings.Join(progress, "\n")
	if strings.Contains(joined, "Merging 1 chunk summary result(s)") {
		t.Fatalf("did not expect merge progress for single successful chunk, got: %s", joined)
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

	result := orch.RunWithMode("execute quarterly reconciliation", "cli")
	if !strings.Contains(strings.ToLower(result), "couldn't understand") {
		t.Fatalf("expected friendly planner fallback result, got %s", result)
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
	if err != nil || !strings.Contains(strings.ToLower(string(finalOutput)), "couldn't understand") {
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

func TestNewOrchestratorForModeUsesChatModelOverride(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)
	t.Setenv("OTTER_ALLOWED_DIRS", t.TempDir())
	t.Setenv("OTTER_MODEL", "")

	if err := settings.Save(settings.Config{
		AllowedDirs: []string{t.TempDir()},
		Model:       "planner-model",
		ChatModel:   "chat-model-fast",
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	orch, err := NewOrchestratorForMode("chat")
	if err != nil {
		t.Fatalf("new orchestrator for chat: %v", err)
	}
	if orch.modelName != "chat-model-fast" {
		t.Fatalf("expected chat model override, got %q", orch.modelName)
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

func TestRunAudioOrganizeChatDryRunThenApprove(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	audioRoot := filepath.Join(home, "Downloads", "audio")
	if err := os.MkdirAll(audioRoot, 0o755); err != nil {
		t.Fatalf("mkdir audio root: %v", err)
	}
	source := filepath.Join(audioRoot, "track.mp3")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed track: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{filepath.Join(home, "Downloads")}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	planReply := orch.RunWithMode("Organize my audio folder", "chat")
	if !strings.Contains(planReply, "Proceed with this plan? [y/N]") {
		t.Fatalf("expected approval prompt, got: %s", planReply)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("dry-run should not move file: %v", err)
	}

	execReply := orch.RunWithMode("yes", "chat")
	if !strings.Contains(strings.ToLower(execReply), "audio organization executed") {
		t.Fatalf("expected execution confirmation, got: %s", execReply)
	}
	candidates := []string{
		filepath.Join(audioRoot, "music", "unknown_music", "track.mp3"),
		filepath.Join(audioRoot, "review", "ambiguous", "track.mp3"),
		filepath.Join(audioRoot, "review", "low_confidence", "track.mp3"),
	}
	found := false
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected moved file after approval in expected destinations")
	}
}

func TestRunAudioOrganizeExecuteWithoutYesOnlyPlans(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	audioRoot := filepath.Join(home, "Downloads", "audio")
	if err := os.MkdirAll(audioRoot, 0o755); err != nil {
		t.Fatalf("mkdir audio root: %v", err)
	}
	source := filepath.Join(audioRoot, "track.mp3")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed track: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{filepath.Join(home, "Downloads")}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}

	reply := orch.RunWithMode("Execute the audio organization", "chat")
	if !strings.Contains(reply, "Proceed with this plan? [y/N]") {
		t.Fatalf("expected explicit confirmation request, got: %s", reply)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("file should remain in place without explicit yes: %v", err)
	}
}

func TestRunCleanupEmptyFoldersFromChat(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "otter", "config.json")
	downloads := filepath.Join(home, "Downloads")
	empty := filepath.Join(downloads, "legacy", "unused")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	reply := orch.RunWithMode("Find empty folders in Downloads", "chat")
	if !strings.Contains(reply, "Detected") || !strings.Contains(reply, "empty folders") {
		t.Fatalf("expected cleanup reply, got: %s", reply)
	}
	if _, err := os.Stat(empty); err != nil {
		t.Fatalf("folder should not be deleted: %v", err)
	}
}

func TestRunCleanupListEmptyFoldersFromChat(t *testing.T) {
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(filepath.Join(downloads, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	reply := orch.RunWithMode("list empty folders in downloads", "chat")
	if !strings.Contains(reply, "Detected") || !strings.Contains(reply, "empty folders") {
		t.Fatalf("expected empty-folders list response, got: %s", reply)
	}
}

func TestRunCleanupStageThemYesUsesPendingRoot(t *testing.T) {
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(filepath.Join(downloads, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))

	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	first := orch.RunWithMode("find empty folders in downloads", "chat")
	if !strings.Contains(first, "Detected") {
		t.Fatalf("expected report reply, got: %s", first)
	}
	second := orch.RunWithMode("stage them yes", "chat")
	if !strings.Contains(second, "Moved") || !strings.Contains(second, "staging") {
		t.Fatalf("expected staged response, got: %s", second)
	}
}

func TestRunCleanupStageInDownloadsYesResolvesNamedPath(t *testing.T) {
	home := t.TempDir()
	downloads := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(filepath.Join(downloads, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	orch, err := NewOrchestrator([]string{downloads}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	reply := orch.RunWithMode("stage empty folders in Downloads yes", "chat")
	if !strings.Contains(reply, "Moved") {
		t.Fatalf("expected staged response, got: %s", reply)
	}
}

func TestCleanupAccessDeniedMessageIncludesAttemptedAndAllowedRoots(t *testing.T) {
	home := t.TempDir()
	allowed := filepath.Join(home, "Downloads", "audio")
	disallowed := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(filepath.Join(disallowed, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir disallowed root: %v", err)
	}
	orch, err := NewOrchestrator([]string{allowed}, stubPlanner{})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	reply := orch.RunWithMode("stage empty folders in "+disallowed+" yes", "chat")
	if !strings.Contains(reply, disallowed) {
		t.Fatalf("expected attempted path in denial, got: %s", reply)
	}
	if !strings.Contains(reply, allowed) {
		t.Fatalf("expected allowed root in denial, got: %s", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "allow access") {
		t.Fatalf("expected actionable grant phrase in denial, got: %s", reply)
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
