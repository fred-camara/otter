package tasks

const PlanVersionV1 = "v1"

type TaskKind string

const (
	TaskOrganizeAudio TaskKind = "organize_audio"
	TaskCleanupEmpty  TaskKind = "cleanup_empty_folders"
)

type TaskSpec struct {
	ID     string            `json:"id,omitempty"`
	Kind   TaskKind          `json:"kind"`
	Root   string            `json:"root"`
	Source string            `json:"source"`
	Flags  map[string]string `json:"flags,omitempty"`
}

type Plan struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	Task       TaskSpec `json:"task"`
	Actions    []Action `json:"actions"`
	Summary    string   `json:"summary,omitempty"`
	ReportPath string   `json:"report_path,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
}

type ActionKind string

const (
	ActionMove   ActionKind = "move"
	ActionStage  ActionKind = "stage"
	ActionReport ActionKind = "report"
	ActionReview ActionKind = "review"
	ActionNoop   ActionKind = "noop"
)

type Action struct {
	ID          string     `json:"id"`
	Kind        ActionKind `json:"kind"`
	Source      string     `json:"source,omitempty"`
	Destination string     `json:"destination,omitempty"`
	Decision    Decision   `json:"decision"`
	Status      string     `json:"status,omitempty"`
}

type Decision struct {
	Classification string   `json:"classification,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	Source         string   `json:"source,omitempty"`
	Evidence       []string `json:"evidence,omitempty"`
	RequiresReview bool     `json:"requires_review,omitempty"`
}
