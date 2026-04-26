package tasks

import (
	"testing"

	"otter/internal/cleanup"
	"otter/internal/organize"
)

func TestFromAudioPlanMapsActionsAndVersion(t *testing.T) {
	source := "/tmp/audio/song.mp3"
	destination := "/tmp/audio/music/song.mp3"
	plan := FromAudioPlan(organize.Plan{
		PlanID:     "audio-1",
		RequestID:  "req-1",
		Profile:    organize.ProfileAudio,
		Root:       "/tmp/audio",
		CreatedAt:  "2026-04-26T00:00:00Z",
		ReportPath: "/tmp/audio/.otter-reports/audio_report.md",
		Actions: []organize.PlanAction{
			{
				SourcePath:      source,
				DestinationPath: destination,
				Classification:  "music/known_artist_title",
				DecisionSource:  "rule",
				Confidence:      0.92,
				Evidence:        []string{"extension is .mp3"},
			},
		},
	}, "summary")

	assertPlanSanity(t, plan, 1)
	if plan.Version != PlanVersionV1 {
		t.Fatalf("expected version %q, got %q", PlanVersionV1, plan.Version)
	}
	if plan.Actions[0].Source != source || plan.Actions[0].Destination != destination {
		t.Fatalf("unexpected action mapping: %#v", plan.Actions[0])
	}
	if plan.Actions[0].Decision.Classification != "music/known_artist_title" {
		t.Fatalf("expected classification to be preserved, got %#v", plan.Actions[0].Decision)
	}
}

func TestFromCleanupReportMapsReportActionAndVersion(t *testing.T) {
	plan := FromCleanupReport("/tmp/Downloads", cleanup.ReportResult{
		Total:      3,
		ReportPath: "/tmp/Downloads/.otter-reports/empty_folders.md",
	}, "summary")

	assertPlanSanity(t, plan, 1)
	if plan.Version != PlanVersionV1 {
		t.Fatalf("expected version %q, got %q", PlanVersionV1, plan.Version)
	}
	if plan.Actions[0].Kind != ActionReport {
		t.Fatalf("expected report action, got %q", plan.Actions[0].Kind)
	}
	if plan.Actions[0].Destination == "" {
		t.Fatalf("expected report destination")
	}
}

func TestFromCleanupStageMapsStageActionsAndVersion(t *testing.T) {
	plan := FromCleanupStage("/tmp/Downloads", cleanup.StageResult{
		StageRoot:  "/tmp/Downloads/empty_folders_staging_20260426-120000",
		ReportPath: "/tmp/Downloads/.otter-reports/empty_folders.md",
		MovedPaths: []string{
			"/tmp/Downloads/a -> /tmp/Downloads/stage/a",
			"/tmp/Downloads/b -> /tmp/Downloads/stage/b",
		},
		Preview: "preview",
	})

	assertPlanSanity(t, plan, 2)
	if plan.Version != PlanVersionV1 {
		t.Fatalf("expected version %q, got %q", PlanVersionV1, plan.Version)
	}
	for _, action := range plan.Actions {
		if action.Kind != ActionStage {
			t.Fatalf("expected stage action, got %q", action.Kind)
		}
		if action.Source == "" || action.Destination == "" {
			t.Fatalf("expected staged source and destination, got %#v", action)
		}
	}
}

func assertPlanSanity(t *testing.T, plan Plan, wantActions int) {
	t.Helper()
	if len(plan.Actions) != wantActions {
		t.Fatalf("expected %d actions, got %d", wantActions, len(plan.Actions))
	}
	seen := map[string]struct{}{}
	for _, action := range plan.Actions {
		if action.ID == "" {
			t.Fatalf("action has empty ID: %#v", action)
		}
		if _, ok := seen[action.ID]; ok {
			t.Fatalf("duplicate action ID %q", action.ID)
		}
		seen[action.ID] = struct{}{}
		if action.Kind == ActionMove && action.Destination == "" {
			t.Fatalf("move action has empty destination: %#v", action)
		}
	}
}
