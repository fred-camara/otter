package agent

import (
	"testing"

	"otter/internal/settings"
)

func TestResolvePlannerModelNameUsesEnvOverride(t *testing.T) {
	cfg := settings.Config{Model: "config-model"}
	name, source := ResolvePlannerModelName(cfg, "env-model")
	if name != "env-model" {
		t.Fatalf("expected env model, got %q", name)
	}
	if source != "environment variable OTTER_MODEL" {
		t.Fatalf("unexpected source: %q", source)
	}
}

func TestResolvePlannerModelNameUsesConfigWhenEnvMissing(t *testing.T) {
	cfg := settings.Config{Model: "config-model"}
	name, source := ResolvePlannerModelName(cfg, "")
	if name != "config-model" {
		t.Fatalf("expected config model, got %q", name)
	}
	if source != "config" {
		t.Fatalf("unexpected source: %q", source)
	}
}

func TestResolvePlannerModelNameFallsBackToDefault(t *testing.T) {
	name, source := ResolvePlannerModelName(settings.Config{}, "")
	if name != DefaultPlannerModelName {
		t.Fatalf("expected default model %q, got %q", DefaultPlannerModelName, name)
	}
	if source != "default" {
		t.Fatalf("unexpected source: %q", source)
	}
}
