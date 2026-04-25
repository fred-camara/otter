package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubModel struct {
	output string
	err    error
	prompt string
}

func (s *stubModel) Generate(prompt string) (string, error) {
	s.prompt = prompt
	if s.err != nil {
		return "", s.err
	}
	return s.output, nil
}

func TestOllamaPlannerPlanBuildsPromptAndReturnsResponse(t *testing.T) {
	model := &stubModel{output: `{"tool":"list_files","input":{"path":"."}}`}
	planner := NewOllamaPlanner(model)

	resp, err := planner.Plan(context.Background(), Request{
		Task:  "list files",
		Tools: []string{"list_files", "read_file"},
	})
	if err != nil {
		t.Fatalf("plan error: %v", err)
	}
	if !strings.Contains(model.prompt, "Use only these tools: list_files, read_file") {
		t.Fatalf("prompt missing tools list: %s", model.prompt)
	}
	if !strings.Contains(model.prompt, "User task: list files") {
		t.Fatalf("prompt missing user task: %s", model.prompt)
	}
	if resp.RawJSON == "" {
		t.Fatalf("expected raw json response")
	}
}

func TestOllamaPlannerPlanPropagatesModelError(t *testing.T) {
	model := &stubModel{err: errors.New("planner down")}
	planner := NewOllamaPlanner(model)

	_, err := planner.Plan(context.Background(), Request{Task: "hello"})
	if err == nil || !strings.Contains(err.Error(), "planner down") {
		t.Fatalf("expected propagated model error, got %v", err)
	}
}

func TestMockPlannerUsesDefaultResponse(t *testing.T) {
	mock := MockPlanner{Response: Response{RawJSON: `{"error":"x"}`}}
	resp, err := mock.Plan(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RawJSON == "" {
		t.Fatalf("expected response")
	}
}

func TestMockPlannerUsesPlanFunc(t *testing.T) {
	called := false
	mock := MockPlanner{
		PlanFunc: func(_ context.Context, req Request) (Response, error) {
			called = true
			if req.Task != "abc" {
				t.Fatalf("unexpected task %q", req.Task)
			}
			return Response{RawJSON: `{"tool":"list_files","input":{}}`}, nil
		},
	}

	_, err := mock.Plan(context.Background(), Request{Task: "abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatalf("expected plan func to be called")
	}
}
