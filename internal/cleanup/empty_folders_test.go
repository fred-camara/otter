package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateEmptyFoldersReport(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "a", "b")
	dsOnly := filepath.Join(root, "tmp")
	nonEmpty := filepath.Join(root, "x")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := os.MkdirAll(dsOnly, 0o755); err != nil {
		t.Fatalf("mkdir dsOnly: %v", err)
	}
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatalf("mkdir nonEmpty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dsOnly, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write .DS_Store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	service := NewService(nil)
	result, err := service.GenerateEmptyFoldersReport(ReportRequest{
		Scopes: []Scope{{Label: "Inside root", Root: root}},
	})
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if result.Total == 0 {
		t.Fatalf("expected non-zero empty folders")
	}
	if _, err := os.Stat(result.ReportPath); err != nil {
		t.Fatalf("report file missing: %v", err)
	}
	if !strings.Contains(result.ReportPath, string(os.PathSeparator)+".otter-reports"+string(os.PathSeparator)) {
		t.Fatalf("expected report in .otter-reports, got %s", result.ReportPath)
	}
	raw, err := os.ReadFile(result.ReportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "No folders were deleted.") {
		t.Fatalf("expected no-delete note, got %q", text)
	}
	if strings.Contains(text, filepath.Join(nonEmpty)) {
		t.Fatalf("non-empty folder should not be reported")
	}
}

func TestStageEmptyFoldersMovesIntoStageDirWithoutDeletingData(t *testing.T) {
	root := t.TempDir()
	emptyA := filepath.Join(root, "a")
	emptyB := filepath.Join(root, "b", "c")
	nonEmpty := filepath.Join(root, "keep")
	if err := os.MkdirAll(emptyA, 0o755); err != nil {
		t.Fatalf("mkdir emptyA: %v", err)
	}
	if err := os.MkdirAll(emptyB, 0o755); err != nil {
		t.Fatalf("mkdir emptyB: %v", err)
	}
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatalf("mkdir nonEmpty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed non-empty file: %v", err)
	}

	service := NewService(nil)
	result, err := service.StageEmptyFolders(StageRequest{Root: root})
	if err != nil {
		t.Fatalf("stage empty folders: %v", err)
	}
	if result.Moved == 0 {
		t.Fatalf("expected moved empty folders")
	}
	if _, err := os.Stat(result.StageRoot); err != nil {
		t.Fatalf("expected stage root: %v", err)
	}
	if _, err := os.Stat(nonEmpty); err != nil {
		t.Fatalf("non-empty folder must remain: %v", err)
	}
}

func TestProtectedPathsAreSkippedFromReportAndStage(t *testing.T) {
	root := t.TempDir()
	protectedApp := filepath.Join(root, "Visual Studio Code.app", "Contents", "Resources", "en.lproj")
	protectedNodeModules := filepath.Join(root, "project", "node_modules", "dep", "empty")
	normalEmpty := filepath.Join(root, "normal", "empty")
	reportsDir := filepath.Join(root, ".otter-reports")
	if err := os.MkdirAll(protectedApp, 0o755); err != nil {
		t.Fatalf("mkdir protected app path: %v", err)
	}
	if err := os.MkdirAll(protectedNodeModules, 0o755); err != nil {
		t.Fatalf("mkdir protected node_modules path: %v", err)
	}
	if err := os.MkdirAll(normalEmpty, 0o755); err != nil {
		t.Fatalf("mkdir normal empty path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(reportsDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir reports dir nested: %v", err)
	}

	service := NewService(nil)
	report, err := service.GenerateEmptyFoldersReport(ReportRequest{
		Scopes: []Scope{{Label: "Inside root", Root: root}},
	})
	if err != nil {
		t.Fatalf("generate report: %v", err)
	}
	if report.ProtectedSkippedTotal == 0 {
		t.Fatalf("expected protected skipped count")
	}
	raw, err := os.ReadFile(report.ReportPath)
	if err != nil {
		t.Fatalf("read report file: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "## Skipped (Protected)") {
		t.Fatalf("expected protected section, got %q", text)
	}
	if strings.Contains(text, protectedApp) {
		t.Fatalf("protected app path should not appear in empty list")
	}
	if strings.Contains(text, protectedNodeModules) {
		t.Fatalf("protected node_modules path should not appear in empty list")
	}
	if !strings.Contains(text, normalEmpty) {
		t.Fatalf("normal empty path should appear")
	}
	if strings.Contains(text, filepath.Join(reportsDir, "nested")) {
		t.Fatalf(".otter-reports should be ignored as protected")
	}

	stage, err := service.StageEmptyFolders(StageRequest{Root: root})
	if err != nil {
		t.Fatalf("stage empty folders: %v", err)
	}
	if stage.ProtectedSkippedTotal == 0 {
		t.Fatalf("expected protected skipped during stage")
	}
	if _, err := os.Stat(protectedApp); err != nil {
		t.Fatalf("protected app path should remain untouched: %v", err)
	}
	if _, err := os.Stat(protectedNodeModules); err != nil {
		t.Fatalf("protected node_modules path should remain untouched: %v", err)
	}
}

func TestPreviewGroupingAndLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		path := filepath.Join(root, "audio", fmt.Sprintf("empty_%d", i))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	service := NewService(nil)
	preview, err := service.PreviewStageEmptyFolders(root)
	if err != nil {
		t.Fatalf("preview stage: %v", err)
	}
	if !strings.Contains(preview.Preview, filepath.ToSlash(filepath.Join(root, "audio"))+"/") {
		t.Fatalf("expected grouped parent in preview, got %q", preview.Preview)
	}
	// Preview limit is 5 items per group.
	count := strings.Count(preview.Preview, "empty_")
	if count > 5 {
		t.Fatalf("expected preview to be limited to <=5 items per group, got %d\n%s", count, preview.Preview)
	}
}

func TestRepeatedRunsDoNotWriteMarkdownInRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	service := NewService(nil)
	_, err := service.GenerateEmptyFoldersReport(ReportRequest{Scopes: []Scope{{Label: "Inside root", Root: root}}})
	if err != nil {
		t.Fatalf("first report: %v", err)
	}
	_, err = service.GenerateEmptyFoldersReport(ReportRequest{Scopes: []Scope{{Label: "Inside root", Root: root}}})
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			t.Fatalf("root polluted with markdown file: %s", entry.Name())
		}
	}
}
