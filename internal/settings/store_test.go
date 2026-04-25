package settings

import (
	"path/filepath"
	"testing"
)

func TestSaveAndLoadConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "otter-config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	input := Config{
		AllowedDirs: []string{"/tmp/a", "/tmp/b"},
	}
	if err := Save(input); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(loaded.AllowedDirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d", len(loaded.AllowedDirs))
	}
}
