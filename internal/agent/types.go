package agent

import "encoding/json"

type ToolCall struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Error string          `json:"error,omitempty"`
}
