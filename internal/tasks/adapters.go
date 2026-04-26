package tasks

import (
	"fmt"
	"path/filepath"
	"strings"

	"otter/internal/cleanup"
	"otter/internal/organize"
)

func FromAudioPlan(plan organize.Plan, summary string) Plan {
	actions := make([]Action, 0, len(plan.Actions))
	for i, action := range plan.Actions {
		actions = append(actions, Action{
			ID:          fmt.Sprintf("%s:%d", plan.PlanID, i),
			Kind:        ActionMove,
			Source:      action.SourcePath,
			Destination: action.DestinationPath,
			Decision: Decision{
				Classification: action.Classification,
				Confidence:     action.Confidence,
				Source:         action.DecisionSource,
				Evidence:       append([]string{}, action.Evidence...),
				RequiresReview: action.RequiresReview,
			},
		})
	}

	return Plan{
		ID:      plan.PlanID,
		Version: PlanVersionV1,
		Task: TaskSpec{
			ID:     plan.RequestID,
			Kind:   TaskOrganizeAudio,
			Root:   plan.Root,
			Source: "service",
			Flags: map[string]string{
				"profile": plan.Profile,
			},
		},
		Actions:    actions,
		Summary:    summary,
		ReportPath: plan.ReportPath,
		CreatedAt:  plan.CreatedAt,
	}
}

func FromCleanupReport(root string, result cleanup.ReportResult, summary string) Plan {
	return Plan{
		ID:      "cleanup-empty-folders-" + filepath.Base(result.ReportPath),
		Version: PlanVersionV1,
		Task: TaskSpec{
			Kind:   TaskCleanupEmpty,
			Root:   root,
			Source: "service",
			Flags: map[string]string{
				"mode": "report",
			},
		},
		Actions: []Action{
			{
				ID:          "report",
				Kind:        ActionReport,
				Destination: result.ReportPath,
				Decision: Decision{
					Source:   "service",
					Evidence: []string{"empty folder scan completed"},
				},
			},
		},
		Summary:    summary,
		ReportPath: result.ReportPath,
	}
}

func FromCleanupStage(root string, result cleanup.StageResult) Plan {
	actions := make([]Action, 0, len(result.MovedPaths))
	for i, movedPath := range result.MovedPaths {
		src, dst := splitMovePair(movedPath)
		actions = append(actions, Action{
			ID:          fmt.Sprintf("stage:%d", i),
			Kind:        ActionStage,
			Source:      src,
			Destination: dst,
			Decision: Decision{
				Source:   "service",
				Evidence: []string{"folder was empty at scan time"},
			},
			Status: "moved",
		})
	}

	return Plan{
		ID:      "cleanup-stage-" + filepath.Base(result.StageRoot),
		Version: PlanVersionV1,
		Task: TaskSpec{
			Kind:   TaskCleanupEmpty,
			Root:   root,
			Source: "service",
			Flags: map[string]string{
				"mode": "stage",
			},
		},
		Actions:    actions,
		Summary:    result.Preview,
		ReportPath: result.ReportPath,
	}
}

func splitMovePair(value string) (string, string) {
	parts := strings.SplitN(value, " -> ", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(value), ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}
