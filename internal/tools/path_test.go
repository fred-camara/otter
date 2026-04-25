package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePathExpandsHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}

	resolved, err := ResolvePath("~/Downloads")
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}

	expectedPrefix := filepath.Join(home, "Downloads")
	if !strings.HasPrefix(resolved, expectedPrefix) {
		t.Fatalf("expected prefix %q, got %q", expectedPrefix, resolved)
	}
}
