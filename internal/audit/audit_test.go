package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartCreatesRunDirectoryAndFiles(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", runsDir)
	t.Setenv("OTTER_AUDIT_DISABLED", "false")

	logger := Start("list files in ~/Downloads", "cli", "qwen")
	if logger == nil || logger.RunDir() == "" {
		t.Fatalf("expected logger with run dir")
	}
	if _, err := os.Stat(logger.RunDir()); err != nil {
		t.Fatalf("run dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logger.RunDir(), "input.txt")); err != nil {
		t.Fatalf("input.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logger.RunDir(), "metadata.json")); err != nil {
		t.Fatalf("metadata.json missing: %v", err)
	}
	logger.LogFinalOutput("done")
	metaRaw, err := os.ReadFile(filepath.Join(logger.RunDir(), "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta Metadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta.Status != "success" {
		t.Fatalf("expected success status, got %q", meta.Status)
	}
}

func TestRunIDUniqueness(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", runsDir)
	t.Setenv("OTTER_AUDIT_DISABLED", "false")

	first := Start("a", "cli", "qwen")
	second := Start("a", "cli", "qwen")
	if first.RunID() == "" || second.RunID() == "" {
		t.Fatalf("expected non-empty run ids")
	}
	if first.RunID() == second.RunID() {
		t.Fatalf("expected unique run ids")
	}
}

func TestInvalidJSONLoggingAndToolCallsAndErrors(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", runsDir)
	t.Setenv("OTTER_AUDIT_DISABLED", "false")

	logger := Start("task", "cli", "qwen")
	logger.LogPlannerResponseRaw(0, strings.Repeat("x", 30000))
	logger.LogError("planner_parse", errors.New("no JSON object found"))
	logger.LogToolCall("list_files", []byte(`{"path":".","content":"secret","Authorization":"Bearer abc"}`), "ok", nil)

	raw, err := os.ReadFile(filepath.Join(logger.RunDir(), "planner_response_raw.txt"))
	if err != nil || !strings.Contains(string(raw), "--- attempt 0") {
		t.Fatalf("expected planner raw output, err=%v body=%q", err, string(raw))
	}
	if len(raw) > 25000 {
		t.Fatalf("expected truncated planner raw output, len=%d", len(raw))
	}
	errs, err := os.ReadFile(filepath.Join(logger.RunDir(), "errors.jsonl"))
	if err != nil || !strings.Contains(string(errs), "planner_parse") {
		t.Fatalf("expected errors entry, err=%v body=%q", err, string(errs))
	}
	calls, err := os.ReadFile(filepath.Join(logger.RunDir(), "tool_calls.jsonl"))
	if err != nil || !strings.Contains(string(calls), "list_files") {
		t.Fatalf("expected tool call entry, err=%v body=%q", err, string(calls))
	}
	if strings.Contains(strings.ToLower(string(calls)), "secret") {
		t.Fatalf("expected redacted content, got %q", string(calls))
	}
	if strings.Contains(strings.ToLower(string(calls)), "bearer abc") {
		t.Fatalf("expected auth redaction, got %q", string(calls))
	}
	logger.LogFinalOutput("failed")
	metaRaw, err := os.ReadFile(filepath.Join(logger.RunDir(), "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta Metadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta.Status != "failure" {
		t.Fatalf("expected failure status, got %q", meta.Status)
	}
}

func TestBestEffortDoesNotPanicOnDisabledLogger(t *testing.T) {
	logger := &Logger{enabled: false}
	logger.LogPlannerRequest(map[string]any{"task": "x"})
	logger.LogPlannerResponseRaw(1, "x")
	logger.LogPlannerResponseParsed(map[string]any{"tool": "list_files"})
	logger.LogToolCall("list_files", []byte(`{}`), "ok", nil)
	logger.LogError("stage", errors.New("boom"))
	logger.LogFinalOutput("done")
}

func TestListRunsAndResolveLatest(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", runsDir)
	t.Setenv("OTTER_AUDIT_DISABLED", "false")

	first := Start("first task", "cli", "qwen")
	first.LogFinalOutput("ok")
	second := Start("second task", "chat", "qwen")
	second.LogError("planner", errors.New("invalid json"))
	second.LogFinalOutput("failed")

	items, err := ListRunSummaries(10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 runs, got %d", len(items))
	}
	if items[0].Dir != second.RunDir() {
		t.Fatalf("expected latest run first")
	}

	latest, err := ResolveRunDirectory("latest")
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}
	if latest != second.RunDir() {
		t.Fatalf("unexpected latest run dir: %s", latest)
	}
}

func TestRunsDirUsesOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "audit-runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", override)
	got, err := RunsDir()
	if err != nil {
		t.Fatalf("RunsDir error: %v", err)
	}
	if got != override {
		t.Fatalf("expected override runs dir %q, got %q", override, got)
	}
}

func TestStartDisabledSkipsWriting(t *testing.T) {
	override := filepath.Join(t.TempDir(), "audit-runs")
	t.Setenv("OTTER_AUDIT_RUNS_DIR", override)
	t.Setenv("OTTER_AUDIT_DISABLED", "true")

	logger := Start("task", "cli", "qwen")
	if logger == nil {
		t.Fatalf("expected logger object")
	}
	if logger.enabled {
		t.Fatalf("expected disabled logger")
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("expected no runs directory creation, stat err=%v", err)
	}
}
