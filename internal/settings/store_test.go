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

func TestSaveAndLoadConfigModel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "otter-config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	input := Config{Model: "qwen2.5-coder:14b"}
	if err := Save(input); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Model != "qwen2.5-coder:14b" {
		t.Fatalf("expected model to persist, got %q", loaded.Model)
	}
}

func TestSaveAndLoadConfigChatModel(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "otter-config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	input := Config{ChatModel: "llama3.1:8b"}
	if err := Save(input); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.ChatModel != "llama3.1:8b" {
		t.Fatalf("expected chat model to persist, got %q", loaded.ChatModel)
	}
}
