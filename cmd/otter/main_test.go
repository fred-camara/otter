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
	if !strings.Contains(text, "Commands: /exit, /help, /undo, /access, /model, /model set <name>") {
		t.Fatalf("expected help text, got %q", text)
	}
	if !strings.Contains(text, "Ollama: not checked yet") {
		t.Fatalf("expected startup ollama status, got %q", text)
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
	if !strings.Contains(readOut.String(), "Current model: llama3.1:8b") {
		t.Fatalf("expected persisted model, got %q", readOut.String())
	}
	if !strings.Contains(readOut.String(), "Source: config") {
		t.Fatalf("expected config source, got %q", readOut.String())
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
	if !strings.Contains(text, "Current model: mistral:7b") {
		t.Fatalf("expected env model, got %q", text)
	}
	if !strings.Contains(text, "Source: environment variable OTTER_MODEL") {
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

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
