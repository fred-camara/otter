package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"otter/internal/tools"
)

const defaultRelativeConfigPath = ".config/otter/config.json"

type Config struct {
	AllowedDirs []string `json:"allowed_dirs"`
}

func Load() (Config, error) {
	configPath, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.AllowedDirs = normalize(cfg.AllowedDirs)
	return cfg, nil
}

func Save(cfg Config) error {
	configPath, err := ConfigPath()
	if err != nil {
		return err
	}

	cfg.AllowedDirs = normalize(cfg.AllowedDirs)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func ConfigPath() (string, error) {
	if customPath := strings.TrimSpace(os.Getenv("OTTER_CONFIG_FILE")); customPath != "" {
		resolved, err := tools.ResolvePath(customPath)
		if err != nil {
			return "", fmt.Errorf("resolve OTTER_CONFIG_FILE: %w", err)
		}
		return resolved, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, defaultRelativeConfigPath), nil
}

func normalize(dirs []string) []string {
	seen := make(map[string]struct{}, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, raw := range dirs {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		resolved, err := tools.ResolvePath(value)
		if err != nil {
			continue
		}
		if _, exists := seen[resolved]; exists {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}
