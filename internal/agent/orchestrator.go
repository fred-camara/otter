package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"otter/internal/model"
	"otter/internal/permissions"
	"otter/internal/settings"
	"otter/internal/tools"
)

const (
	defaultModelName = "qwen2.5-coder:14b"
	defaultOllamaURL = "http://127.0.0.1:11434"
	maxPlanRetries   = 2
)

type Orchestrator struct {
	registry    *tools.Registry
	planner     Planner
	allowedDirs []string
}

type llmPlanner struct {
	model model.Interface
}

func NewOrchestrator(allowedDirs []string, planner Planner) (*Orchestrator, error) {
	normalizedDirs, err := normalizeAllowedDirs(allowedDirs)
	if err != nil {
		return nil, err
	}

	listTool, err := tools.NewListFilesTool(normalizedDirs)
	if err != nil {
		return nil, err
	}
	readTool := tools.NewReadFileTool(normalizedDirs)
	summarizeTool := tools.NewSummarizeFilesTool(normalizedDirs)

	return &Orchestrator{
		registry:    tools.NewRegistry(listTool, readTool, summarizeTool),
		planner:     planner,
		allowedDirs: normalizedDirs,
	}, nil
}

func NewOrchestratorFromEnv() (*Orchestrator, error) {
	var err error
	envDirs := parseAllowedDirs(os.Getenv("OTTER_ALLOWED_DIRS"))
	cfg, cfgErr := settings.Load()
	savedDirs := []string{}
	if cfgErr == nil {
		savedDirs = cfg.AllowedDirs
	}

	dirs := mergeDirs(envDirs, savedDirs)
	if len(dirs) == 0 {
		dirs, err = defaultAllowedDirs()
		if err != nil {
			return nil, err
		}
	}

	modelName := firstNonEmpty(strings.TrimSpace(os.Getenv("OTTER_MODEL")), defaultModelName)
	ollamaURL := firstNonEmpty(strings.TrimSpace(os.Getenv("OTTER_OLLAMA_URL")), defaultOllamaURL)
	planner := &llmPlanner{model: model.NewOllama(modelName, ollamaURL)}

	return NewOrchestrator(dirs, planner)
}

func RunTask(task string) string {
	orch, err := NewOrchestratorFromEnv()
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't initialize tools: %v", err)
	}
	return orch.Run(task)
}

func (o *Orchestrator) Run(task string) string {
	if o.planner == nil {
		return fmt.Sprintf("🦦 Stub: I received your task: %q", task)
	}

	if directCall, ok := o.directToolCallForTask(task); ok {
		return o.executeToolCall(task, directCall)
	}

	toolNames, err := o.registry.Names()
	if err != nil {
		return fmt.Sprintf("🦦 Could not load tools: %v", err)
	}

	var parsed ToolCall
	for attempt := 0; attempt <= maxPlanRetries; attempt++ {
		raw, err := o.planner.Plan(task, toolNames)
		if err != nil {
			return fmt.Sprintf("🦦 Planner error: %v", err)
		}

		parsed, err = parseToolCall(raw)
		if err != nil {
			if attempt == maxPlanRetries {
				return fmt.Sprintf("🦦 Planner returned invalid JSON after retries: %v", err)
			}
			continue
		}
		if strings.TrimSpace(parsed.Error) != "" {
			return "🦦 " + parsed.Error
		}
		parsed.Tool = normalizeToolName(parsed.Tool)
		if strings.TrimSpace(parsed.Tool) == "" {
			if attempt == maxPlanRetries {
				return "🦦 Planner did not select a tool."
			}
			continue
		}
		break
	}

	return o.executeToolCall(task, parsed)
}

func parseToolCall(raw string) (ToolCall, error) {
	var parsed ToolCall
	trimmed := strings.TrimSpace(raw)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		return parsed, nil
	}

	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return ToolCall{}, fmt.Errorf("no JSON object found")
	}

	candidate := trimmed[start : end+1]
	if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
		return ToolCall{}, err
	}
	return parsed, nil
}

func parseAllowedDirs(raw string) []string {
	parts := strings.Split(raw, ",")
	dirs := make([]string, 0, len(parts))
	for _, part := range parts {
		candidate := strings.TrimSpace(part)
		if candidate == "" {
			continue
		}
		dirs = append(dirs, filepath.Clean(candidate))
	}
	return dirs
}

func normalizeAllowedDirs(allowedDirs []string) ([]string, error) {
	normalized := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		absDir, err := tools.ResolvePath(dir)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed directory %q: %w", dir, err)
		}
		normalized = append(normalized, absDir)
	}
	if len(normalized) == 0 {
		return nil, fmt.Errorf("at least one allowed directory is required")
	}
	return normalized, nil
}

func defaultAllowedDirs() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get working directory: %w", err)
	}

	dirs := []string{cwd}
	home, err := os.UserHomeDir()
	if err != nil {
		return dirs, nil
	}

	candidates := []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "notes"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			dirs = append(dirs, candidate)
		}
	}
	return dirs, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (p *llmPlanner) Plan(task string, toolNames []string) (string, error) {
	prompt := buildPrompt(task, toolNames)
	return p.model.Generate(prompt)
}

func buildPrompt(task string, toolNames []string) string {
	return fmt.Sprintf(`You are Otter, a local system agent.
Return valid JSON only with this exact schema:
{"tool":"tool_name","input":{}}
If task cannot be completed safely, return:
{"error":"reason"}

Rules:
- Use only these tools: %s
- Never delete files
- Never use shell commands
- Prefer safe read/list/summarize
- No markdown, no explanations
- Do not include any text before or after the JSON object

User task: %s`, strings.Join(toolNames, ", "), task)
}

func normalizeToolName(value string) string {
	normalized := strings.Trim(value, " \t\r\n`\"'")
	normalized = strings.TrimPrefix(normalized, "/")
	switch strings.ToLower(normalized) {
	case "ls", "list", "listfiles", "list_files_tool":
		return "list_files"
	case "file_explorer", "directory_listing", "scan_files":
		return "list_files"
	case "cat", "read", "readfile", "read_file_tool":
		return "read_file"
	case "read_notes", "open_file":
		return "read_file"
	case "summary", "summarize", "summarise", "summarizefiles", "summarize_file":
		return "summarize_files"
	case "summarize_notes":
		return "summarize_files"
	}
	return normalized
}

func coerceListFilesInput(raw json.RawMessage, task string) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" && trimmed != "{}" && trimmed != "\"{}\"" && trimmed != "\"null\"" {
		return raw
	}

	pathValue := "."
	lowered := strings.ToLower(task)
	if index := strings.LastIndex(lowered, " in "); index >= 0 {
		candidate := strings.TrimSpace(task[index+4:])
		if candidate != "" {
			pathValue = sanitizeTaskPath(candidate)
		}
	}

	payload, err := json.Marshal(map[string]string{"path": pathValue})
	if err != nil {
		return raw
	}
	return payload
}

func (o *Orchestrator) directToolCallForTask(task string) (ToolCall, bool) {
	trimmed := strings.TrimSpace(task)
	lowered := strings.ToLower(trimmed)

	if isHelpRequest(lowered) {
		return ToolCall{Error: o.helpMessage()}, true
	}
	if strings.Contains(lowered, "apple notes") && strings.Contains(lowered, "access") {
		return ToolCall{Error: appleNotesGuidance()}, true
	}
	if isAccessListRequest(lowered) {
		return ToolCall{Error: o.listAccessMessage()}, true
	}
	if isAccessAddRequest(lowered) {
		message, err := o.addAccessFromTask(trimmed)
		if err != nil {
			return ToolCall{Error: err.Error()}, true
		}
		return ToolCall{Error: message}, true
	}

	if strings.Contains(lowered, "latest notes") || (strings.Contains(lowered, "notes") && strings.Contains(lowered, "last") && strings.Contains(lowered, "day")) {
		days := extractDaysWindow(lowered, 10)
		paths, err := collectRecentNotePaths(o.allowedDirs, days, 20)
		if err != nil {
			return ToolCall{Error: fmt.Sprintf("I couldn't scan your notes safely: %v", err)}, true
		}
		if len(paths) == 0 {
			return ToolCall{Error: fmt.Sprintf("I couldn't find note files from the last %d days in allowed directories.\n\n%s", days, notesAccessGuidance())}, true
		}
		input, _ := json.Marshal(map[string][]string{"paths": paths})
		return ToolCall{Tool: "summarize_files", Input: input}, true
	}

	if strings.HasPrefix(lowered, "list files") {
		pathValue := "."
		if strings.Contains(lowered, " in ") {
			index := strings.LastIndex(lowered, " in ")
			candidate := strings.TrimSpace(trimmed[index+4:])
			if candidate != "" {
				pathValue = candidate
			}
		}
		input, _ := json.Marshal(map[string]string{"path": pathValue})
		return ToolCall{Tool: "list_files", Input: input}, true
	}

	if strings.HasPrefix(lowered, "read ") {
		pathValue := strings.TrimSpace(trimmed[len("read "):])
		if pathValue != "" {
			input, _ := json.Marshal(map[string]string{"path": pathValue})
			return ToolCall{Tool: "read_file", Input: input}, true
		}
	}

	return ToolCall{}, false
}

func (o *Orchestrator) executeToolCall(task string, parsed ToolCall) string {
	if strings.TrimSpace(parsed.Error) != "" {
		return "🦦 " + parsed.Error
	}

	if permissions.LevelForTool(parsed.Tool) == permissions.LevelForbidden {
		if inferred := inferSafeToolFromTask(task); inferred != "" {
			parsed.Tool = inferred
		}
	}

	if err := permissions.Validate(parsed.Tool); err != nil {
		return fmt.Sprintf("🦦 Permission denied: %v\n\n%s", err, o.accessGuidance())
	}

	if parsed.Tool == "list_files" {
		parsed.Input = coerceListFilesInput(parsed.Input, task)
	}
	if parsed.Tool == "read_file" {
		parsed.Input = coerceReadFileInput(parsed.Input, task)
	}

	result, err := o.registry.Execute(parsed.Tool, parsed.Input)
	if err != nil {
		if parsed.Tool == "list_files" {
			fallbackInput := coerceListFilesInput(json.RawMessage("{}"), task)
			fallbackResult, fallbackErr := o.registry.Execute(parsed.Tool, fallbackInput)
			if fallbackErr == nil {
				return "🦦 " + fallbackResult
			}
		}
		if parsed.Tool == "read_file" && inferSafeToolFromTask(task) == "list_files" {
			fallbackInput := coerceListFilesInput(json.RawMessage("{}"), task)
			fallbackResult, fallbackErr := o.registry.Execute("list_files", fallbackInput)
			if fallbackErr == nil {
				return "🦦 " + fallbackResult
			}
		}
		if strings.Contains(strings.ToLower(err.Error()), "outside allowed directories") {
			return fmt.Sprintf("🦦 %v\n\n%s", err, o.accessGuidance())
		}
		return fmt.Sprintf("🦦 Tool error: %v", err)
	}

	return "🦦 " + result
}

func inferSafeToolFromTask(task string) string {
	lower := strings.ToLower(task)
	switch {
	case strings.Contains(lower, "list"), strings.Contains(lower, "files in"), strings.Contains(lower, "show files"), strings.Contains(lower, "folder"):
		return "list_files"
	case strings.Contains(lower, "read"), strings.Contains(lower, "open"):
		return "read_file"
	case strings.Contains(lower, "summar"), strings.Contains(lower, "latest notes"):
		return "summarize_files"
	default:
		return ""
	}
}

func coerceReadFileInput(raw json.RawMessage, task string) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" && trimmed != "{}" && trimmed != "\"{}\"" && trimmed != "\"null\"" {
		return raw
	}

	lower := strings.ToLower(task)
	pathValue := ""
	if index := strings.LastIndex(lower, " in "); index >= 0 {
		pathValue = sanitizeTaskPath(strings.TrimSpace(task[index+4:]))
	} else {
		pathValue = sanitizeTaskPath(strings.TrimSpace(strings.TrimPrefix(task, "read ")))
	}
	if pathValue == "" {
		return raw
	}

	payload, err := json.Marshal(map[string]string{"path": pathValue})
	if err != nil {
		return raw
	}
	return payload
}

func sanitizeTaskPath(value string) string {
	clean := strings.TrimSpace(value)
	clean = strings.Trim(clean, "\"'`")
	clean = strings.TrimRight(clean, "?.!,;:")
	return clean
}

func extractDaysWindow(taskLower string, fallback int) int {
	fields := strings.Fields(taskLower)
	for index, token := range fields {
		if token == "last" || token == "over" {
			for seek := index + 1; seek < len(fields) && seek <= index+4; seek++ {
				value, err := strconv.Atoi(strings.Trim(fields[seek], " ,.;:"))
				if err == nil && value > 0 && value <= 365 {
					return value
				}
			}
		}
	}
	return fallback
}

type noteFile struct {
	path    string
	modTime time.Time
}

func collectRecentNotePaths(allowedDirs []string, days, limit int) ([]string, error) {
	if days <= 0 {
		days = 10
	}
	if limit <= 0 {
		limit = 20
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	files := make([]noteFile, 0, 32)
	seenPaths := make(map[string]struct{}, 64)

	searchDirs := noteLikelyDirs(allowedDirs)
	if len(searchDirs) == 0 {
		searchDirs = allowedDirs
	}

	for _, allowedDir := range searchDirs {
		broadFallback := !isLikelyNotesDir(allowedDir)
		err := filepath.WalkDir(allowedDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".md" && ext != ".txt" {
				return nil
			}
			if broadFallback && !looksLikeNoteFile(path) {
				return nil
			}

			info, infoErr := entry.Info()
			if infoErr != nil {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				return nil
			}

			cleanPath := filepath.Clean(path)
			if _, exists := seenPaths[cleanPath]; exists {
				return nil
			}
			seenPaths[cleanPath] = struct{}{}

			files = append(files, noteFile{
				path:    cleanPath,
				modTime: info.ModTime(),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.After(files[j].modTime)
	})

	if len(files) > limit {
		files = files[:limit]
	}

	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths, nil
}

func noteLikelyDirs(allowedDirs []string) []string {
	dirs := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		if isLikelyNotesDir(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func isLikelyNotesDir(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.Contains(base, "note") ||
		strings.Contains(base, "journal") ||
		strings.Contains(base, "second brain")
}

func looksLikeNoteFile(path string) bool {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "changelog") || strings.Contains(lower, "readme") {
		return false
	}
	keywords := []string{"note", "journal", "meeting", "todo", "daily"}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func isHelpRequest(taskLower string) bool {
	return taskLower == "help" ||
		strings.Contains(taskLower, "what can you do") ||
		strings.Contains(taskLower, "how do i use") ||
		strings.Contains(taskLower, "how to use otter")
}

func isAccessListRequest(taskLower string) bool {
	return strings.Contains(taskLower, "what directories") ||
		strings.Contains(taskLower, "what folders") ||
		strings.Contains(taskLower, "what can otter access") ||
		strings.Contains(taskLower, "show access")
}

func isAccessAddRequest(taskLower string) bool {
	return strings.Contains(taskLower, "have access to") ||
		strings.Contains(taskLower, "allow access to") ||
		strings.Contains(taskLower, "grant access to") ||
		strings.Contains(taskLower, "add access to") ||
		strings.Contains(taskLower, "give otter access") ||
		(strings.Contains(taskLower, "access") && strings.Contains(taskLower, "notes"))
}

func (o *Orchestrator) listAccessMessage() string {
	if len(o.allowedDirs) == 0 {
		return "Otter currently has no allowed directories configured.\n\n" + o.accessGuidance()
	}
	lines := make([]string, 0, len(o.allowedDirs))
	for _, dir := range o.allowedDirs {
		lines = append(lines, "- "+dir)
	}
	return "Current allowed directories:\n" + strings.Join(lines, "\n")
}

func (o *Orchestrator) addAccessFromTask(task string) (string, error) {
	requested, err := extractAccessTargets(task)
	if err != nil {
		return "", err
	}
	if len(requested) == 0 {
		return "", fmt.Errorf("I couldn't detect any valid directories in that request. Try: otter \"give otter access to ~/Desktop and ~/Documents\"")
	}

	cfg, err := settings.Load()
	if err != nil {
		return "", fmt.Errorf("failed to load otter settings: %w", err)
	}
	merged := mergeDirs(o.allowedDirs, cfg.AllowedDirs)

	added := make([]string, 0, len(requested))
	alreadyAllowed := make([]string, 0, len(requested))
	for _, candidate := range requested {
		resolved, resolveErr := tools.ResolvePath(candidate)
		if resolveErr != nil {
			continue
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if !containsDir(merged, resolved) {
			merged = append(merged, resolved)
			added = append(added, resolved)
		} else {
			alreadyAllowed = append(alreadyAllowed, resolved)
		}
	}

	if len(added) == 0 {
		if len(alreadyAllowed) > 0 {
			lines := make([]string, 0, len(alreadyAllowed))
			for _, dir := range alreadyAllowed {
				lines = append(lines, "- "+dir)
			}
			return "Those directories are already allowed:\n" + strings.Join(lines, "\n"), nil
		}
		if strings.Contains(strings.ToLower(task), "notes") {
			return "", fmt.Errorf("I couldn't find a notes directory to add.\n\n%s", notesAccessGuidance())
		}
		return "", fmt.Errorf("I couldn't add any existing directories from that request.\n\n%s", o.accessGuidance())
	}

	cfg.AllowedDirs = merged
	if err := settings.Save(cfg); err != nil {
		return "", fmt.Errorf("failed to save otter settings: %w", err)
	}

	o.allowedDirs = merged
	lines := make([]string, 0, len(added))
	for _, dir := range added {
		lines = append(lines, "- "+dir)
	}
	return "Added directory access:\n" + strings.Join(lines, "\n"), nil
}

func (o *Orchestrator) accessGuidance() string {
	return "You can grant access with prompts like:\n" +
		"- otter \"give otter access to Desktop and Documents\"\n" +
		"- otter \"allow access to ~/Work\"\n" +
		"- otter \"give otter access to notes\"\n" +
		"Then verify with:\n" +
		"- otter \"what directories can otter access?\""
}

func notesAccessGuidance() string {
	return "Try one of these:\n" +
		"- otter \"give otter access to ~/notes\"\n" +
		"- otter \"give otter access to ~/Documents/Notes\"\n" +
		"- otter \"give otter access to ~/Library/Mobile Documents/com~apple~CloudDocs/Notes\"\n\n" +
		"Otter currently reads note files from folders (.md/.txt). Apple Notes app database access is not implemented yet."
}

func appleNotesGuidance() string {
	return "Otter currently reads note files from folders (.md/.txt), not the Apple Notes app database.\n\n" + notesAccessGuidance()
}

func (o *Orchestrator) helpMessage() string {
	return "I can currently:\n" +
		"- list files in a folder\n" +
		"- read text files\n" +
		"- summarize note files\n\n" +
		"Examples:\n" +
		"- otter \"list files in ~/Downloads\"\n" +
		"- otter \"read ~/notes/today.md\"\n" +
		"- otter \"Read my latest notes over the last 10 days\"\n\n" +
		o.accessGuidance()
}

func extractAccessTargets(task string) ([]string, error) {
	lower := strings.ToLower(task)
	normalized := strings.NewReplacer(",", " ", ";", " ", " and ", " ", " or ", " ").Replace(task)
	parts := strings.Fields(normalized)

	targets := make([]string, 0, 8)
	for _, token := range parts {
		clean := strings.Trim(token, "\"'`")
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "~/") || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "$HOME/") {
			targets = append(targets, clean)
		}
	}

	homeShortcuts := map[string]string{
		"desktop":     "~/Desktop",
		"documents":   "~/Documents",
		"downloads":   "~/Downloads",
		"notes":       "~/notes",
		"apple notes": "~/Library/Mobile Documents/com~apple~CloudDocs/Notes",
		"pictures":    "~/Pictures",
		"music":       "~/Music",
		"movies":      "~/Movies",
	}
	for keyword, path := range homeShortcuts {
		if strings.Contains(lower, keyword) {
			targets = append(targets, path)
		}
	}

	if strings.Contains(lower, "notes") {
		targets = append(targets,
			"~/notes",
			"~/Documents/Notes",
			"~/Documents/notes",
			"~/Library/Mobile Documents/com~apple~CloudDocs/Notes",
		)
	}

	return uniqueStrings(targets), nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func mergeDirs(groups ...[]string) []string {
	merged := make([]string, 0)
	for _, group := range groups {
		for _, item := range group {
			if !containsDir(merged, item) {
				merged = append(merged, item)
			}
		}
	}
	return merged
}

func containsDir(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
