package organize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratePlanAudioClassifiesAndWritesArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	context := filepath.Dir(root)
	if err := os.MkdirAll(filepath.Join(root, "messy"), 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "James Brown - Living in America.mp3"), []byte("a"), 0o644); err != nil {
		t.Fatalf("seed mp3: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "Stickz 808 C.wav"), []byte("b"), 0o644); err != nil {
		t.Fatalf("seed wav: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "38.wav"), []byte("c"), 0o644); err != nil {
		t.Fatalf("seed ambiguous wav: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("not-audio"), 0o644); err != nil {
		t.Fatalf("seed text: %v", err)
	}

	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{
		Profile:     ProfileAudio,
		Root:        root,
		ContextRoot: context,
		RequestID:   "req-test-1",
	})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	if len(plan.Actions) != 3 {
		t.Fatalf("expected 3 planned audio actions, got %d", len(plan.Actions))
	}
	if _, err := os.Stat(plan.PlanPath); err != nil {
		t.Fatalf("expected plan file: %v", err)
	}
	if _, err := os.Stat(plan.ReportPath); err != nil {
		t.Fatalf("expected report file: %v", err)
	}
	text := service.SummarizePlan(plan)
	if !strings.Contains(text, "Audio organization plan") {
		t.Fatalf("expected summary title, got %q", text)
	}
	if !strings.Contains(text, "Nothing outside "+root) {
		t.Fatalf("expected scope statement, got %q", text)
	}
	if !strings.Contains(text, "Preview (sample of planned moves)") {
		t.Fatalf("expected preview block, got %q", text)
	}
	if !strings.Contains(text, "Confidence breakdown") {
		t.Fatalf("expected confidence breakdown, got %q", text)
	}
}

func TestExecutePlanRequiresApproval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	source := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{Profile: ProfileAudio, Root: root, ContextRoot: filepath.Dir(root)})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	_, err = service.ExecutePlan(ExecutePlanRequest{Plan: plan, Approved: false})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "explicit approval") {
		t.Fatalf("expected approval error, got %v", err)
	}
}

func TestExecutePlanSkipsChangedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	source := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(source, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{Profile: ProfileAudio, Root: root, ContextRoot: filepath.Dir(root)})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	if err := os.WriteFile(source, []byte("changed"), 0o644); err != nil {
		t.Fatalf("change source: %v", err)
	}
	result, err := service.ExecutePlan(ExecutePlanRequest{Plan: plan, Approved: true})
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	if result.Executed != 0 {
		t.Fatalf("expected no executed moves for changed source, got %d", result.Executed)
	}
	if result.Skipped == 0 {
		t.Fatalf("expected skipped count for changed source")
	}
}

func TestExecutePlanWritesEmptyFolderReportAndDoesNotDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	contextRoot := filepath.Join(filepath.Dir(root), "Downloads")
	outsideRoot := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.MkdirAll(contextRoot, 0o755); err != nil {
		t.Fatalf("mkdir context root: %v", err)
	}
	if err := os.MkdirAll(outsideRoot, 0o755); err != nil {
		t.Fatalf("mkdir outside root: %v", err)
	}
	emptyInRoot := filepath.Join(root, "old_pack", "kicks")
	dsStoreOnly := filepath.Join(root, "tmp")
	emptyInContext := filepath.Join(contextRoot, "legacy", "unused")
	if err := os.MkdirAll(emptyInRoot, 0o755); err != nil {
		t.Fatalf("mkdir emptyInRoot: %v", err)
	}
	if err := os.MkdirAll(dsStoreOnly, 0o755); err != nil {
		t.Fatalf("mkdir dsStoreOnly: %v", err)
	}
	if err := os.MkdirAll(emptyInContext, 0o755); err != nil {
		t.Fatalf("mkdir emptyInContext: %v", err)
	}
	outsideEmpty := filepath.Join(outsideRoot, "should_not_be_reported")
	if err := os.MkdirAll(outsideEmpty, 0o755); err != nil {
		t.Fatalf("mkdir outsideEmpty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dsStoreOnly, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write .DS_Store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed song: %v", err)
	}
	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{Profile: ProfileAudio, Root: root, ContextRoot: contextRoot})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	result, err := service.ExecutePlan(ExecutePlanRequest{Plan: plan, Approved: true})
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}
	if result.EmptyFolderCount == 0 {
		t.Fatalf("expected empty folders report count")
	}
	if _, err := os.Stat(result.EmptyFolderReportPath); err != nil {
		t.Fatalf("expected empty folder report file: %v", err)
	}
	if _, err := os.Stat(emptyInRoot); err != nil {
		t.Fatalf("empty root folder should not be deleted: %v", err)
	}
	if _, err := os.Stat(dsStoreOnly); err != nil {
		t.Fatalf("ds_store-only folder should not be deleted: %v", err)
	}
	if _, err := os.Stat(emptyInContext); err != nil {
		t.Fatalf("empty context folder should not be deleted: %v", err)
	}
	reportRaw, err := os.ReadFile(result.EmptyFolderReportPath)
	if err != nil {
		t.Fatalf("read empty report: %v", err)
	}
	report := string(reportRaw)
	if !strings.Contains(report, "No folders were deleted.") {
		t.Fatalf("expected safety note, got %q", report)
	}
	if !strings.Contains(report, emptyInRoot) {
		t.Fatalf("expected root empty folder path in report")
	}
	if strings.Contains(report, outsideEmpty) {
		t.Fatalf("outside scope folder should not be in report")
	}
}

func TestPendingPlanRoundTrip(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cfg", "config.json")
	t.Setenv("OTTER_CONFIG_FILE", configPath)

	service := NewService(nil)
	p := Plan{
		PlanID: "audio-1",
		Root:   "/tmp/audio",
	}
	if err := service.SavePendingPlan(p); err != nil {
		t.Fatalf("save pending: %v", err)
	}
	pending, err := service.LoadPendingPlan()
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if pending.Plan.PlanID != "audio-1" {
		t.Fatalf("unexpected pending plan id: %s", pending.Plan.PlanID)
	}
	if err := service.ClearPendingPlan(); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	_, err = service.LoadPendingPlan()
	if err == nil {
		t.Fatalf("expected error after clear pending")
	}
}

func TestPlanJSONLIncludesDecisionSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed song: %v", err)
	}
	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{Profile: ProfileAudio, Root: root, ContextRoot: filepath.Dir(root)})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	raw, err := os.ReadFile(plan.PlanPath)
	if err != nil {
		t.Fatalf("read plan jsonl: %v", err)
	}
	if !strings.Contains(string(raw), `"decision_source"`) {
		t.Fatalf("expected decision_source in plan file, got %s", string(raw))
	}
}

func TestInspectPlanGroupsByDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "audio")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed song: %v", err)
	}
	service := NewService(nil)
	plan, err := service.GeneratePlan(GeneratePlanRequest{Profile: ProfileAudio, Root: root, ContextRoot: filepath.Dir(root)})
	if err != nil {
		t.Fatalf("generate plan: %v", err)
	}
	text, err := service.InspectPlan(plan.PlanPath, "")
	if err != nil {
		t.Fatalf("inspect plan: %v", err)
	}
	if !strings.Contains(text, "confidence:") {
		t.Fatalf("expected confidence details, got %q", text)
	}
	if !strings.Contains(text, "source:") {
		t.Fatalf("expected decision source details, got %q", text)
	}
}
