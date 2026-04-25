package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type WriteFileTool struct {
	allowedDirs []string
}

type writeFileInput struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite"`
	Confirm   bool   `json:"confirm"`
}

func NewWriteFileTool(allowedDirs []string) *WriteFileTool {
	return &WriteFileTool{allowedDirs: allowedDirs}
}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Description() string {
	return "Writes generated text to a file in allowed directories."
}

func (t *WriteFileTool) Execute(input json.RawMessage) (string, error) {
	var req writeFileInput
	if err := json.Unmarshal(input, &req); err != nil {
		return "", errors.New("invalid write_file input")
	}

	pathValue := strings.TrimSpace(req.Path)
	if pathValue == "" {
		return "", errors.New("path is required")
	}

	absPath, err := ResolvePath(pathValue)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if !isPathAllowed(absPath, t.allowedDirs) {
		return "", errors.New("path is outside allowed directories")
	}
	if isHiddenPath(absPath) {
		return "", errors.New("hidden paths are not allowed")
	}

	info, statErr := os.Stat(absPath)
	if statErr == nil && info.IsDir() {
		return "", errors.New("target path is a directory")
	}

	if statErr == nil && !req.Overwrite {
		return "", errors.New("target file already exists; set overwrite=true with confirm=true to replace it")
	}
	if statErr == nil && req.Overwrite && !req.Confirm {
		return "", errors.New("overwrite requires confirm=true")
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", fmt.Errorf("create parent directory: %w", err)
	}
	if err := os.WriteFile(absPath, []byte(req.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return fmt.Sprintf("Wrote file: %s", absPath), nil
}

type MoveFileTool struct {
	allowedDirs []string
}

type moveOp struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type moveFileInput struct {
	Source  string   `json:"source"`
	Target  string   `json:"target"`
	Moves   []moveOp `json:"moves"`
	Confirm bool     `json:"confirm"`
}

func NewMoveFileTool(allowedDirs []string) *MoveFileTool {
	return &MoveFileTool{allowedDirs: allowedDirs}
}

func (t *MoveFileTool) Name() string {
	return "move_file"
}

func (t *MoveFileTool) Description() string {
	return "Moves files safely without overwriting existing targets."
}

func (t *MoveFileTool) Execute(input json.RawMessage) (string, error) {
	var req moveFileInput
	if err := json.Unmarshal(input, &req); err != nil {
		return "", errors.New("invalid move_file input")
	}

	moves := normalizeMoves(req)
	if len(moves) == 0 {
		return "", errors.New("source/target or moves is required")
	}

	planned, err := t.validateMoves(moves)
	if err != nil {
		return "", err
	}

	if len(planned) > 1 && !req.Confirm {
		lines := make([]string, 0, len(planned))
		for _, op := range planned {
			lines = append(lines, fmt.Sprintf("- %s -> %s", op.Source, op.Target))
		}
		return "Dry-run move plan:\n" + strings.Join(lines, "\n") + "\nRe-run with confirm=true to execute.", nil
	}

	for _, op := range planned {
		if err := os.MkdirAll(filepath.Dir(op.Target), 0o755); err != nil {
			return "", fmt.Errorf("create parent directory for %s: %w", op.Target, err)
		}
		if err := os.Rename(op.Source, op.Target); err != nil {
			return "", fmt.Errorf("move %s -> %s: %w", op.Source, op.Target, err)
		}
	}

	lines := make([]string, 0, len(planned))
	for _, op := range planned {
		lines = append(lines, fmt.Sprintf("- %s -> %s", op.Source, op.Target))
	}
	return "Moved files:\n" + strings.Join(lines, "\n"), nil
}

func normalizeMoves(req moveFileInput) []moveOp {
	ops := make([]moveOp, 0, len(req.Moves)+1)
	if strings.TrimSpace(req.Source) != "" || strings.TrimSpace(req.Target) != "" {
		ops = append(ops, moveOp{
			Source: strings.TrimSpace(req.Source),
			Target: strings.TrimSpace(req.Target),
		})
	}
	for _, move := range req.Moves {
		ops = append(ops, moveOp{
			Source: strings.TrimSpace(move.Source),
			Target: strings.TrimSpace(move.Target),
		})
	}
	return ops
}

func (t *MoveFileTool) validateMoves(ops []moveOp) ([]moveOp, error) {
	valid := make([]moveOp, 0, len(ops))
	for _, op := range ops {
		if op.Source == "" || op.Target == "" {
			return nil, errors.New("each move requires source and target")
		}

		sourcePath, err := ResolvePath(op.Source)
		if err != nil {
			return nil, fmt.Errorf("resolve source path: %w", err)
		}
		targetPath, err := ResolvePath(op.Target)
		if err != nil {
			return nil, fmt.Errorf("resolve target path: %w", err)
		}

		if !isPathAllowed(sourcePath, t.allowedDirs) || !isPathAllowed(targetPath, t.allowedDirs) {
			return nil, errors.New("move paths must stay within allowed directories")
		}
		if isHiddenPath(sourcePath) || isHiddenPath(targetPath) {
			return nil, errors.New("hidden paths are not allowed")
		}

		sourceInfo, statErr := os.Stat(sourcePath)
		if statErr != nil {
			return nil, fmt.Errorf("source file not found: %s", sourcePath)
		}
		if sourceInfo.IsDir() {
			return nil, fmt.Errorf("source must be a file: %s", sourcePath)
		}
		if _, targetErr := os.Stat(targetPath); targetErr == nil {
			return nil, fmt.Errorf("target already exists: %s", targetPath)
		}

		valid = append(valid, moveOp{
			Source: sourcePath,
			Target: targetPath,
		})
	}
	return valid, nil
}
