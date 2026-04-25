package agent

import "encoding/json"

type Planner interface {
	Plan(task string, tools []string) (string, error)
}

type ToolCall struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Error string          `json:"error,omitempty"`
}
