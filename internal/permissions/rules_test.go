package permissions

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateToolCallRejectsForbiddenTool(t *testing.T) {
	err := ValidateToolCall("delete_file", nil)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected forbidden tool error, got %v", err)
	}
}

func TestValidateToolCallRequiresConfirmForOverwrite(t *testing.T) {
	input, _ := json.Marshal(map[string]any{
		"path":      "/tmp/report.md",
		"overwrite": true,
	})
	err := ValidateToolCall("write_file", input)
	if err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("expected overwrite confirmation error, got %v", err)
	}
}

func TestValidateToolCallAllowsCreateOnlyWrite(t *testing.T) {
	input, _ := json.Marshal(map[string]any{
		"path":    "/tmp/report.md",
		"content": "new report",
	})
	if err := ValidateToolCall("write_file", input); err != nil {
		t.Fatalf("expected write_file to pass, got %v", err)
	}
}

func TestValidateToolCallAllowsBatchMoveForDryRun(t *testing.T) {
	input, _ := json.Marshal(map[string]any{
		"moves": []map[string]string{
			{"source": "/tmp/a", "target": "/tmp/b"},
			{"source": "/tmp/c", "target": "/tmp/d"},
		},
	})
	if err := ValidateToolCall("move_file", input); err != nil {
		t.Fatalf("expected batch move to pass for dry-run, got %v", err)
	}
}

func TestValidateToolCallAllowsSingleMoveWithoutConfirm(t *testing.T) {
	input, _ := json.Marshal(map[string]any{
		"source": "/tmp/a",
		"target": "/tmp/b",
	})
	if err := ValidateToolCall("move_file", input); err != nil {
		t.Fatalf("expected single move to pass, got %v", err)
	}
}
