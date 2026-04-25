package permissions

import "fmt"

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
	level := LevelForTool(tool)
	switch level {
	case LevelSafe:
		return nil
	case LevelConfirm:
		return fmt.Errorf("tool %q requires confirmation and is disabled in this milestone", tool)
	default:
		return fmt.Errorf("tool %q is forbidden", tool)
	}
}
