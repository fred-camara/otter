package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLogEntries(t *testing.T) {
	text := "/old/a.wav -> /new/a.wav\ninvalid\n/old/b.wav -> /new/b.wav"
	entries := ParseLogEntries(text)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Source != "/old/a.wav" || entries[0].Target != "/new/a.wav" {
		t.Fatalf("unexpected first entry: %#v", entries[0])
	}
}

func TestGenerateIsDryRunOnly(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "take.wav")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	plan, err := Generate(root, nil)
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	if !plan.DryRun {
		t.Fatalf("expected dry run")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file should remain untouched: %v", err)
	}
}

func TestGenerateJSONValidity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	plan, err := Generate(root, nil)
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	text, err := PlanJSON(plan)
	if err != nil {
		t.Fatalf("plan json: %v", err)
	}

	var parsed Plan
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("json should be valid: %v", err)
	}
	if len(parsed.Entries) != 1 {
		t.Fatalf("expected one entry")
	}
}

func TestGenerateLowConfidenceNeedsReview(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "weirdfile.zzz")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	plan, err := Generate(root, nil)
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected one entry")
	}
	entry := plan.Entries[0]
	if entry.Confidence != ConfidenceLow {
		t.Fatalf("expected low confidence, got %s", entry.Confidence)
	}
	if filepath.Base(filepath.Dir(entry.ProposedDestination)) != "Needs Review" {
		t.Fatalf("expected Needs Review destination, got %s", entry.ProposedDestination)
	}
}

func TestGenerateDeterminism(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mix"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "mix", "Cymatics - Kick.wav"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	logs := []LogEntry{{Source: filepath.Join(root, "old", "Cymatics - Kick.wav"), Target: filepath.Join(root, "mix", "Cymatics - Kick.wav")}}

	first, err := Generate(root, logs)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := Generate(root, logs)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}

	j1, err := PlanJSON(first)
	if err != nil {
		t.Fatalf("first json: %v", err)
	}
	j2, err := PlanJSON(second)
	if err != nil {
		t.Fatalf("second json: %v", err)
	}
	if j1 != j2 {
		t.Fatalf("expected deterministic output")
	}
}

func TestGenerateUsesLogsAsHighConfidence(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "organized", "a.wav")
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(current, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	logs := []LogEntry{{Source: filepath.Join(root, "original", "pack1", "a.wav"), Target: current}}

	plan, err := Generate(root, logs)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected one entry")
	}
	if plan.Entries[0].Confidence != ConfidenceHigh {
		t.Fatalf("expected high confidence from log, got %s", plan.Entries[0].Confidence)
	}
	if plan.Entries[0].ProposedDestination != filepath.Join(root, "original", "pack1", "a.wav") {
		t.Fatalf("unexpected proposed destination: %s", plan.Entries[0].ProposedDestination)
	}
}
