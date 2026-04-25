package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type MoveHistory struct {
	Task      string       `json:"task"`
	CreatedAt time.Time    `json:"created_at"`
	Moves     []MoveRecord `json:"moves"`
}

type MoveRecord struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

func MoveHistoryPath() (string, error) {
	configPath, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(configPath), "move_history.json"), nil
}

func LoadMoveHistory() (MoveHistory, error) {
	path, err := MoveHistoryPath()
	if err != nil {
		return MoveHistory{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MoveHistory{}, nil
		}
		return MoveHistory{}, fmt.Errorf("read move history: %w", err)
	}

	var history MoveHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return MoveHistory{}, fmt.Errorf("parse move history: %w", err)
	}
	return history, nil
}

func SaveMoveHistory(history MoveHistory) error {
	path, err := MoveHistoryPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal move history: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create move history dir: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write move history: %w", err)
	}
	return nil
}

func ClearMoveHistory() error {
	path, err := MoveHistoryPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear move history: %w", err)
	}
	return nil
}
