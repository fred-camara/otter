package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"otter/internal/audit"
)

func TestRunChatREPLCommands(t *testing.T) {
	t.Setenv("OTTER_OLLAMA_URL", "http://127.0.0.1:1")
	in := strings.NewReader("/help\n/undo\n/access\n/exit\n")
	var out bytes.Buffer
	calls := make([]string, 0, 2)
	run := func(task string) string {
		calls = append(calls, task)
		return "ok:" + task
	}

	if err := runChatREPL(in, &out, run); err != nil {
		t.Fatalf("runChatREPL error: %v", err)
	}

	expected := []string{"undo last move", "what directories can otter access?"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("unexpected dispatched tasks. got=%v want=%v", calls, expected)
	}
	text := out.String()
	if !strings.Contains(text, "Commands: /help, /access, /undo, /model, /exit") {
		t.Fatalf("expected help text, got %q", text)
	}
	if !strings.Contains(text, "Hello, I’m Otter. What can I help you with?") {
		t.Fatalf("expected welcome greeting, got %q", text)
	}
	if !strings.Contains(text, "🦦 Otter — local-first ops") {
		t.Fatalf("expected branded title, got %q", text)
	}
	if !strings.Contains(text, "Type /help for commands • /exit to quit") {
		t.Fatalf("expected command hint line, got %q", text)
	}
	if !strings.Contains(text, "Ollama: not checked yet") {
		t.Fatalf("expected startup ollama status, got %q", text)
	}
}

func TestRenderChatHeaderNonTTYNoAnimationOrANSI(t *testing.T) {
	var out bytes.Buffer
	renderChatHeader(&out)
	text := out.String()
	if strings.Contains(text, "\r") {
		t.Fatalf("did not expect carriage-return animation in non-tty output: %q", text)
	}
	if strings.Contains(text, "\x1b[") {
		t.Fatalf("did not expect ANSI escapes in non-tty output: %q", text)
	}
}

func TestRenderChatHeaderNoColorWhenNO_COLORSet(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	renderChatHeader(&out)
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("expected no ANSI escapes with NO_COLOR set, got %q", out.String())
	}
}

func TestRenderChatHeaderNoColorWhenTermDumb(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var out bytes.Buffer
	renderChatHeader(&out)
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("expected no ANSI escapes with TERM=dumb, got %q", out.String())
	}
}

func TestAppendChatTurnKeepsLastFive(t *testing.T) {
	history := make([]chatTurn, 0, 6)
	for i := 0; i < 6; i++ {
		history = appendChatTurn(history, chatTurn{user: string(rune('a' + i))})
	}
	if len(history) != 5 {
		t.Fatalf("expected 5 turns, got %d", len(history))
	}
	if history[0].user != "b" || history[4].user != "f" {
		t.Fatalf("unexpected kept history: %#v", history)
	}
}

func TestShouldInjectContext(t *testing.T) {
	if !shouldInjectContext("do that again") {
		t.Fatalf("expected context injection for pronoun task")
	}
	if shouldInjectContext("list files in ~/Downloads") {
		t.Fatalf("did not expect context injection")
	}
}

func TestQuickOllamaStatusAvailable(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/api/tags" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"models":[{"name":"qwen3.5:latest"}]}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}
	status := quickOllamaStatusWithClient("http://127.0.0.1:11434", client)
	if status != "available" {
		t.Fatalf("expected available, got %q", status)
	}
}

func TestQuickOllamaStatusUnavailableOnRequestError(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("dial timeout")
		}),
	}
	status := quickOllamaStatusWithClient("http://127.0.0.1:11434", client)
	if status != "unavailable" {
		t.Fatalf("expected unavailable, got %q", status)
	}
}

func TestListOllamaModelNamesWithClient(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			body := `{"models":[{"name":"qwen3.5:latest"},{"name":"llama3.1:8b"},{"name":"qwen3.5:latest"}]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	models, err := listOllamaModelNamesWithClient("http://127.0.0.1:11434", client)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(models) != 2 || models[0] != "llama3.1:8b" || models[1] != "qwen3.5:latest" {
		t.Fatalf("unexpected models: %#v", models)
	}
}

func TestHandleModelCommandSetUpdatesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	var out bytes.Buffer
	if err := handleModelCommand([]string{"set", "llama3.1:8b"}, &out); err != nil {
		t.Fatalf("handleModelCommand set: %v", err)
	}

	text := out.String()
	if !strings.Contains(text, "Saved model in config: llama3.1:8b") {
		t.Fatalf("unexpected output: %q", text)
	}
	if !strings.Contains(text, "ollama pull llama3.1:8b") {
		t.Fatalf("expected pull suggestion, got: %q", text)
	}

	var readOut bytes.Buffer
	if err := handleModelCommand(nil, &readOut); err != nil {
		t.Fatalf("handleModelCommand read: %v", err)
	}
	if !strings.Contains(readOut.String(), "Main model: llama3.1:8b") {
		t.Fatalf("expected persisted main model, got %q", readOut.String())
	}
	if !strings.Contains(readOut.String(), "Main source: config") {
		t.Fatalf("expected config source, got %q", readOut.String())
	}
	if !strings.Contains(readOut.String(), "Chat model: llama3.1:8b") {
		t.Fatalf("expected chat fallback to main model when chat_model unset, got %q", readOut.String())
	}
}

func TestHandleModelCommandPrefersEnvSource(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	var out bytes.Buffer
	if err := handleModelCommand([]string{"set", "qwen2.5-coder:14b"}, &out); err != nil {
		t.Fatalf("set config model: %v", err)
	}
	t.Setenv("OTTER_MODEL", "mistral:7b")

	var readOut bytes.Buffer
	if err := handleModelCommand(nil, &readOut); err != nil {
		t.Fatalf("handleModelCommand read: %v", err)
	}
	text := readOut.String()
	if !strings.Contains(text, "Main model: mistral:7b") {
		t.Fatalf("expected env model, got %q", text)
	}
	if !strings.Contains(text, "Main source: environment variable OTTER_MODEL") {
		t.Fatalf("expected env source, got %q", text)
	}
}

func TestHandleModelCommandUsageError(t *testing.T) {
	var out bytes.Buffer
	err := handleModelCommand([]string{"set"}, &out)
	if err == nil || !strings.Contains(err.Error(), "usage: otter model") {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestRunChatREPLModelSetDoesNotDispatchPlanner(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)
	t.Setenv("OTTER_OLLAMA_URL", "http://127.0.0.1:1")

	in := strings.NewReader("/model set qwen3.5:latest\n/exit\n")
	var out bytes.Buffer
	calls := make([]string, 0, 1)
	run := func(task string) string {
		calls = append(calls, task)
		return "stub:" + task
	}

	if err := runChatREPL(in, &out, run); err != nil {
		t.Fatalf("runChatREPL error: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("model command should not dispatch planner, got calls=%v", calls)
	}
	if !strings.Contains(out.String(), "Saved model in config: qwen3.5:latest") {
		t.Fatalf("expected set-model response, got %q", out.String())
	}
}

func TestHandleModelCommandSetChatAndShow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	var out bytes.Buffer
	if err := handleModelCommand([]string{"set", "chat", "llama3.1:8b"}, &out); err != nil {
		t.Fatalf("set chat model: %v", err)
	}
	if !strings.Contains(out.String(), "Saved chat model in config: llama3.1:8b") {
		t.Fatalf("unexpected set chat output: %q", out.String())
	}

	var showOut bytes.Buffer
	if err := handleModelCommand([]string{"show"}, &showOut); err != nil {
		t.Fatalf("model show: %v", err)
	}
	if !strings.Contains(showOut.String(), "Chat model: llama3.1:8b") {
		t.Fatalf("expected chat model in show output, got %q", showOut.String())
	}
}

func TestRunChatLoopIgnoresEscapeSequenceInput(t *testing.T) {
	editor := &scriptedEditor{lines: []string{"\x1b[A", "/exit"}}
	var out bytes.Buffer
	calls := []string{}
	run := func(task string) string {
		calls = append(calls, task)
		return "ok"
	}
	if err := runChatLoop(editor, &out, run); err != nil {
		t.Fatalf("runChatLoop error: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no dispatched tasks for escape input, got %v", calls)
	}
	if strings.Contains(out.String(), "\x1b[A") {
		t.Fatalf("should not echo escape sequence, got %q", out.String())
	}
}

func TestHandleRunsCommandListsRuns(t *testing.T) {
	t.Setenv("OTTER_AUDIT_DISABLED", "false")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	t.Setenv("OTTER_AUDIT_RUNS_DIR", filepath.Join(home, ".config", "otter", "runs"))

	first := audit.Start("list files", "cli", "qwen")
	first.LogFinalOutput("ok")

	var out bytes.Buffer
	if err := handleRunsCommand(&out); err != nil {
		t.Fatalf("handleRunsCommand error: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, first.RunID()) {
		t.Fatalf("expected run id in runs list, got %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "success") {
		t.Fatalf("expected status in runs list, got %q", text)
	}
}

func TestHandleShowRunLatestWorks(t *testing.T) {
	t.Setenv("OTTER_AUDIT_DISABLED", "false")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OTTER_CONFIG_FILE", filepath.Join(home, ".config", "otter", "config.json"))
	t.Setenv("OTTER_AUDIT_RUNS_DIR", filepath.Join(home, ".config", "otter", "runs"))

	run := audit.Start("organize downloads", "chat", "qwen")
	run.LogError("planner_parse", errors.New("no JSON object found"))
	run.LogFinalOutput("failed")

	var out bytes.Buffer
	if err := handleShowRunCommand("latest", &out); err != nil {
		t.Fatalf("handleShowRunCommand error: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "Input: organize downloads") {
		t.Fatalf("expected input line, got %q", text)
	}
	if !strings.Contains(text, "Status: failure") {
		t.Fatalf("expected failure status, got %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "planner_parse") {
		t.Fatalf("expected key error in output, got %q", text)
	}
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type scriptedEditor struct {
	lines []string
	index int
}

func (s *scriptedEditor) Readline() (string, error) {
	if s.index >= len(s.lines) {
		return "", io.EOF
	}
	value := s.lines[s.index]
	s.index++
	return value, nil
}

func (s *scriptedEditor) Close() error { return nil }
