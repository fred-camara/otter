package permissions

import (
	"encoding/json"
	"fmt"
)

const (
	LevelSafe      = "safe"
	LevelConfirm   = "confirm"
	LevelForbidden = "forbidden"
)

func LevelForTool(tool string) string {
	switch tool {
	case "list_files", "read_file", "summarize_files":
		return LevelSafe
	case "write_file", "move_file":
		return LevelConfirm
	default:
		return LevelForbidden
	}
}

func Validate(tool string) error {
	return ValidateToolCall(tool, nil)
}

func ValidateToolCall(tool string, input json.RawMessage) error {
	level := LevelForTool(tool)
	switch level {
	case LevelSafe:
		return nil
	case LevelConfirm:
		return validateConfirmTool(tool, input)
	default:
		return fmt.Errorf("tool %q is forbidden", tool)
	}
}

func validateConfirmTool(tool string, input json.RawMessage) error {
	switch tool {
	case "write_file":
		var payload map[string]any
		if len(input) > 0 {
			_ = json.Unmarshal(input, &payload)
		}
		overwrite := boolValue(payload, "overwrite")
		confirm := boolValue(payload, "confirm")
		if overwrite && !confirm {
			return fmt.Errorf("overwrite requires explicit confirmation")
		}
		return nil
	case "move_file":
		return nil
	default:
		return fmt.Errorf("tool %q requires confirmation and is disabled", tool)
	}
}

func boolValue(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key]
	if !ok {
		return false
	}
	flag, ok := value.(bool)
	return ok && flag
}
