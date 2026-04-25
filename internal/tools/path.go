package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ResolvePath(raw string) (string, error) {
	pathValue := strings.TrimSpace(raw)
	if pathValue == "" {
		return "", fmt.Errorf("path is required")
	}

	pathValue = os.ExpandEnv(pathValue)
	expanded, err := expandHome(pathValue)
	if err != nil {
		return "", err
	}

	absPath, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absPath), nil
}

func expandHome(pathValue string) (string, error) {
	if pathValue == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home path: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(pathValue, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home path: %w", err)
		}
		return filepath.Join(home, pathValue[2:]), nil
	}
	return pathValue, nil
}
