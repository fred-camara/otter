package cleanup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"otter/internal/audit"
	"otter/internal/settings"
	"otter/internal/tools"
)

const previewPerGroup = 5

type Service struct {
	audit *audit.Logger
	now   func() time.Time
}

type Scope struct {
	Label string
	Root  string
}

type ReportRequest struct {
	Scopes    []Scope
	OutputDir string
}

type Group struct {
	Label   string
	Root    string
	Folders []string
}

type ReportResult struct {
	Total                  int
	ReportPath             string
	Groups                 []Group
	ProtectedSkippedTotal  int
	ProtectedSkippedByType map[string]int
	ProtectedExamples      []string
}

type StageRequest struct {
	Root      string
	StageRoot string
}

type StageResult struct {
	TotalFound             int
	Moved                  int
	StageRoot              string
	ReportPath             string
	MovedPaths             []string
	ProtectedSkippedTotal  int
	ProtectedSkippedByType map[string]int
	ProtectedExamples      []string
	Preview                string
}

type scanResult struct {
	emptyFolders      []string
	protectedByType   map[string]int
	protectedExamples []string
}

func NewService(a *audit.Logger) *Service {
	return &Service{
		audit: a,
		now:   time.Now,
	}
}

func (s *Service) GenerateEmptyFoldersReport(req ReportRequest) (ReportResult, error) {
	scopes := make([]Scope, 0, len(req.Scopes))
	for _, scope := range req.Scopes {
		root := strings.TrimSpace(scope.Root)
		if root == "" {
			continue
		}
		resolved, err := tools.ResolvePath(root)
		if err != nil {
			return ReportResult{}, fmt.Errorf("resolve scope root %q: %w", root, err)
		}
		label := strings.TrimSpace(scope.Label)
		if label == "" {
			label = "Inside root"
		}
		scopes = append(scopes, Scope{Label: label, Root: resolved})
	}
	if len(scopes) == 0 {
		return ReportResult{}, fmt.Errorf("at least one scope root is required")
	}

	outDir := strings.TrimSpace(req.OutputDir)
	var err error
	if outDir == "" {
		outDir, err = ReportDirForRoot(scopes[0].Root)
		if err != nil {
			return ReportResult{}, err
		}
	}
	outDirResolved, err := tools.ResolvePath(outDir)
	if err != nil {
		return ReportResult{}, fmt.Errorf("resolve output dir: %w", err)
	}
	if err := os.MkdirAll(outDirResolved, 0o755); err != nil {
		return ReportResult{}, fmt.Errorf("create output dir: %w", err)
	}

	groups := make([]Group, 0, len(scopes))
	protectedByType := map[string]int{}
	protectedExamples := make([]string, 0, 8)
	total := 0
	for _, scope := range scopes {
		scan, err := findEmptyFoldersInScope(scope.Root)
		if err != nil {
			return ReportResult{}, err
		}
		total += len(scan.emptyFolders)
		groups = append(groups, Group{
			Label:   scope.Label,
			Root:    scope.Root,
			Folders: scan.emptyFolders,
		})
		for k, v := range scan.protectedByType {
			protectedByType[k] += v
		}
		protectedExamples = appendUniqueProtectedExamples(protectedExamples, scan.protectedExamples, 10)
	}

	reportPath := filepath.Join(outDirResolved, fmt.Sprintf("empty_folders_%s.md", s.now().UTC().Format("20060102-150405")))
	if err := writeReportMarkdown(reportPath, total, groups, protectedByType, protectedExamples); err != nil {
		return ReportResult{}, err
	}
	if s.audit != nil {
		s.audit.LogToolCall("cleanup_empty_folders_report", nil, reportPath, nil)
	}

	return ReportResult{
		Total:                  total,
		ReportPath:             reportPath,
		Groups:                 groups,
		ProtectedSkippedTotal:  sumMapValues(protectedByType),
		ProtectedSkippedByType: protectedByType,
		ProtectedExamples:      protectedExamples,
	}, nil
}

func (s *Service) StageEmptyFolders(req StageRequest) (StageResult, error) {
	root := strings.TrimSpace(req.Root)
	if root == "" {
		root = "~/Downloads"
	}
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return StageResult{}, fmt.Errorf("resolve root: %w", err)
	}
	stageRoot := strings.TrimSpace(req.StageRoot)
	if stageRoot == "" {
		stageRoot = filepath.Join(resolvedRoot, fmt.Sprintf("empty_folders_staging_%s", s.now().UTC().Format("20060102-150405")))
	}
	resolvedStageRoot, err := tools.ResolvePath(stageRoot)
	if err != nil {
		return StageResult{}, fmt.Errorf("resolve stage root: %w", err)
	}
	if !pathWithinRoot(resolvedStageRoot, resolvedRoot) {
		return StageResult{}, fmt.Errorf("stage root must be inside cleanup root")
	}

	scan, err := findEmptyFoldersInScope(resolvedRoot)
	if err != nil {
		return StageResult{}, err
	}
	selected := selectTopLevelFolders(scan.emptyFolders)
	preview := buildStagePreview(resolvedRoot, selected, scan.protectedByType, scan.protectedExamples)

	if err := os.MkdirAll(resolvedStageRoot, 0o755); err != nil {
		return StageResult{}, fmt.Errorf("create stage root: %w", err)
	}
	moved := make([]string, 0, len(selected))
	for _, src := range selected {
		if !pathWithinRoot(src, resolvedRoot) {
			continue
		}
		rel, relErr := filepath.Rel(resolvedRoot, src)
		if relErr != nil {
			continue
		}
		dest := filepath.Join(resolvedStageRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			continue
		}
		if _, err := os.Stat(dest); err == nil {
			dest = uniquePath(dest)
		}
		if err := os.Rename(src, dest); err != nil {
			continue
		}
		moved = append(moved, fmt.Sprintf("%s -> %s", src, dest))
	}

	report, err := s.GenerateEmptyFoldersReport(ReportRequest{
		Scopes: []Scope{{Label: "Inside root", Root: resolvedRoot}},
	})
	if err != nil {
		return StageResult{}, err
	}
	if s.audit != nil {
		s.audit.LogToolCall("cleanup_empty_folders_stage", nil, resolvedStageRoot, nil)
	}
	return StageResult{
		TotalFound:             len(scan.emptyFolders),
		Moved:                  len(moved),
		StageRoot:              resolvedStageRoot,
		ReportPath:             report.ReportPath,
		MovedPaths:             moved,
		ProtectedSkippedTotal:  sumMapValues(scan.protectedByType),
		ProtectedSkippedByType: scan.protectedByType,
		ProtectedExamples:      scan.protectedExamples,
		Preview:                preview,
	}, nil
}

func (s *Service) PreviewStageEmptyFolders(root string) (StageResult, error) {
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return StageResult{}, err
	}
	scan, err := findEmptyFoldersInScope(resolvedRoot)
	if err != nil {
		return StageResult{}, err
	}
	selected := selectTopLevelFolders(scan.emptyFolders)
	preview := buildStagePreview(resolvedRoot, selected, scan.protectedByType, scan.protectedExamples)
	return StageResult{
		TotalFound:             len(scan.emptyFolders),
		Moved:                  len(selected),
		ProtectedSkippedTotal:  sumMapValues(scan.protectedByType),
		ProtectedSkippedByType: scan.protectedByType,
		ProtectedExamples:      scan.protectedExamples,
		Preview:                preview,
	}, nil
}

func SummarizeReport(result ReportResult) string {
	root := ""
	if len(result.Groups) > 0 {
		root = result.Groups[0].Root
	}
	lines := []string{
		"Empty folder plan",
		"",
		"Root:",
		root,
		"",
		fmt.Sprintf("Total empty folders: %d", result.Total),
		fmt.Sprintf("Will stage: %d", result.Total),
		fmt.Sprintf("Skipped: %d (protected)", result.ProtectedSkippedTotal),
		"",
		"Preview:",
		"",
	}
	for _, group := range result.Groups {
		previewGroups := groupByTopLevel(group.Root, group.Folders)
		parentKeys := sortedKeys(previewGroups)
		for _, parent := range parentKeys {
			lines = append(lines, parent+"/", "")
			items := previewGroups[parent]
			limit := previewPerGroup
			if len(items) < limit {
				limit = len(items)
			}
			for i := 0; i < limit; i++ {
				lines = append(lines, "* "+items[i])
				lines = append(lines, "  reason: folder contains no files")
			}
			lines = append(lines, "")
		}
	}
	if result.ProtectedSkippedTotal > 0 {
		lines = append(lines, "Skipped (protected):", "")
		limit := len(result.ProtectedExamples)
		if limit > previewPerGroup {
			limit = previewPerGroup
		}
		for i := 0; i < limit; i++ {
			lines = append(lines, result.ProtectedExamples[i])
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func buildStagePreview(root string, toStage []string, protectedByType map[string]int, protectedExamples []string) string {
	grouped := groupByTopLevel(root, toStage)
	lines := []string{
		"Empty folder plan",
		"",
		"Root:",
		root,
		"",
		fmt.Sprintf("Total empty folders: %d", len(toStage)),
		fmt.Sprintf("Will stage: %d", len(toStage)),
		fmt.Sprintf("Skipped: %d (protected)", sumMapValues(protectedByType)),
		"",
		"Preview:",
		"",
	}
	for _, parent := range sortedKeys(grouped) {
		lines = append(lines, parent+"/", "")
		items := grouped[parent]
		limit := previewPerGroup
		if len(items) < limit {
			limit = len(items)
		}
		for i := 0; i < limit; i++ {
			lines = append(lines, "* "+items[i])
			lines = append(lines, "  reason: folder contains no files")
		}
		lines = append(lines, "")
	}
	if sumMapValues(protectedByType) > 0 {
		lines = append(lines, "Skipped (protected):", "")
		limit := len(protectedExamples)
		if limit > previewPerGroup {
			limit = previewPerGroup
		}
		for i := 0; i < limit; i++ {
			lines = append(lines, protectedExamples[i])
		}
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func writeReportMarkdown(path string, total int, groups []Group, protectedByType map[string]int, protectedExamples []string) error {
	var builder strings.Builder
	builder.WriteString("# Empty Folder Report\n\n")
	builder.WriteString(fmt.Sprintf("Total empty folders found: %d\n\n", total))
	for _, group := range groups {
		builder.WriteString(fmt.Sprintf("## %s: %s\n", group.Label, group.Root))
		if len(group.Folders) == 0 {
			builder.WriteString("- none\n\n")
			continue
		}
		grouped := groupByTopLevel(group.Root, group.Folders)
		for _, parent := range sortedKeys(grouped) {
			builder.WriteString(fmt.Sprintf("### %s\n", parent))
			for _, rel := range grouped[parent] {
				builder.WriteString("- " + filepath.Join(group.Root, rel) + "\n")
			}
			builder.WriteString("\n")
		}
	}

	builder.WriteString("## Skipped (Protected)\n\n")
	if sumMapValues(protectedByType) == 0 {
		builder.WriteString("- none\n\n")
	} else {
		builder.WriteString(fmt.Sprintf("Skipped %d folders inside protected containers:\n", sumMapValues(protectedByType)))
		for _, key := range sortedMapKeys(protectedByType) {
			builder.WriteString(fmt.Sprintf("- %d inside %s\n", protectedByType[key], key))
		}
		builder.WriteString("\nExamples:\n")
		for _, ex := range protectedExamples {
			builder.WriteString("- " + ex + "\n")
		}
		builder.WriteString("\n")
	}
	builder.WriteString("These folders are inside application bundles or system directories and were not modified.\n\n")
	builder.WriteString("No folders were deleted.\n")
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func findEmptyFoldersInScope(scopeRoot string) (scanResult, error) {
	scopeRoot = filepath.Clean(scopeRoot)
	info, err := os.Stat(scopeRoot)
	if err != nil {
		return scanResult{}, fmt.Errorf("stat scope root %s: %w", scopeRoot, err)
	}
	if !info.IsDir() {
		return scanResult{}, fmt.Errorf("scope root is not a directory: %s", scopeRoot)
	}

	out := make([]string, 0, 64)
	protectedByType := map[string]int{}
	protectedExamples := make([]string, 0, 10)
	err = filepath.WalkDir(scopeRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if path == scopeRoot {
			return nil
		}
		if protectedType, protected := protectedContainerType(entry.Name()); protected {
			protectedByType[protectedType]++
			protectedExamples = appendUniqueProtectedExamples(protectedExamples, []string{filepath.Clean(path) + "/..."}, 10)
			return filepath.SkipDir
		}

		empty, err := isEffectivelyEmptyDir(path)
		if err != nil {
			return nil
		}
		if empty {
			out = append(out, filepath.Clean(path))
		}
		return nil
	})
	if err != nil {
		return scanResult{}, err
	}
	sort.Strings(out)
	return scanResult{
		emptyFolders:      out,
		protectedByType:   protectedByType,
		protectedExamples: protectedExamples,
	}, nil
}

func protectedContainerType(name string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "", false
	}
	ext := strings.ToLower(filepath.Ext(lower))
	switch ext {
	case ".app", ".framework", ".bundle", ".pkg", ".kext", ".plugin", ".appex", ".xpc":
		return ext, true
	case ".photoslibrary", ".musiclibrary", ".imovielibrary", ".logicx", ".band", ".fcpxbundle":
		return ext, true
	}
	switch lower {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "venv", ".venv", ".otter-reports":
		return lower, true
	}
	if lower == "__pycache__" || strings.Contains(lower, "__pycache__") {
		return "__pycache__", true
	}
	return "", false
}

func isEffectivelyEmptyDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return false, nil
		}
		if strings.TrimSpace(entry.Name()) != ".DS_Store" {
			return false, nil
		}
	}
	return true, nil
}

func groupByTopLevel(root string, folders []string) map[string][]string {
	grouped := make(map[string][]string, 16)
	for _, abs := range folders {
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "../") || rel == ".." || rel == "." {
			continue
		}
		parts := strings.Split(rel, "/")
		top := parts[0]
		parent := filepath.ToSlash(filepath.Join(root, top))
		grouped[parent] = append(grouped[parent], rel)
	}
	for key := range grouped {
		sort.Strings(grouped[key])
	}
	return grouped
}

func selectTopLevelFolders(paths []string) []string {
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	selected := make([]string, 0, len(sorted))
	for _, path := range sorted {
		skip := false
		for _, root := range selected {
			if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
				skip = true
				break
			}
		}
		if !skip {
			selected = append(selected, path)
		}
	}
	return selected
}

func uniquePath(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for index := 1; ; index++ {
		candidate := fmt.Sprintf("%s_%d%s", base, index, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func pathWithinRoot(path, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if cleanPath == cleanRoot {
		return true
	}
	return strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator))
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sumMapValues(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

func appendUniqueProtectedExamples(existing, incoming []string, limit int) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item] = struct{}{}
	}
	out := append([]string{}, existing...)
	for _, item := range incoming {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		out = append(out, item)
		seen[item] = struct{}{}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func ReportDirForRoot(root string) (string, error) {
	cleanRoot, err := tools.ResolvePath(root)
	if err != nil {
		return "", err
	}
	if override := strings.TrimSpace(os.Getenv("OTTER_REPORTS_ROOT")); override != "" {
		resolvedOverride, err := tools.ResolvePath(override)
		if err != nil {
			return "", fmt.Errorf("resolve OTTER_REPORTS_ROOT: %w", err)
		}
		return filepath.Join(resolvedOverride, safeRootLabel(cleanRoot)), nil
	}
	_, err = settings.ConfigPath()
	if err != nil {
		return filepath.Join(cleanRoot, ".otter-reports"), nil
	}
	return filepath.Join(cleanRoot, ".otter-reports"), nil
}

func safeRootLabel(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if strings.TrimSpace(base) == "" || base == "." || base == string(os.PathSeparator) {
		base = "root"
	}
	replacer := strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_")
	return replacer.Replace(base)
}
