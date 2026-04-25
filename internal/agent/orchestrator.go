package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
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
	writeTool := tools.NewWriteFileTool(normalizedDirs)
	moveTool := tools.NewMoveFileTool(normalizedDirs)

	return &Orchestrator{
		registry:    tools.NewRegistry(listTool, readTool, summarizeTool, writeTool, moveTool),
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

	if response, handled := o.handleComposedTask(task); handled {
		return response
	}

	if response, handled := o.handleUndoTask(task); handled {
		return response
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
- Never use network or external APIs
- Prefer safe read/list/summarize/write/move
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
	case "write", "writefile", "create_file":
		return "write_file"
	case "move", "mv", "movefile":
		return "move_file"
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
	if isOrganizeDownloadsRequest(lowered) {
		return o.planOrganizeDownloads(task)
	}
	if strings.Contains(lowered, "write a summary report to ") || strings.Contains(lowered, "write summary report to ") {
		call, err := o.summaryReportToolCall(task)
		if err != nil {
			return ToolCall{Error: err.Error()}, true
		}
		return call, true
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

	if err := permissions.ValidateToolCall(parsed.Tool, parsed.Input); err != nil {
		return fmt.Sprintf("🦦 Permission denied: %v\n\n%s", err, o.accessGuidance())
	}

	if parsed.Tool == "list_files" {
		parsed.Input = coerceListFilesInput(parsed.Input, task)
	}
	if parsed.Tool == "read_file" {
		parsed.Input = coerceReadFileInput(parsed.Input, task)
	}

	result, err := o.runTool(parsed.Tool, parsed.Input)
	if err != nil {
		if parsed.Tool == "list_files" {
			fallbackInput := coerceListFilesInput(json.RawMessage("{}"), task)
			fallbackResult, fallbackErr := o.runTool(parsed.Tool, fallbackInput)
			if fallbackErr == nil {
				return "🦦 " + fallbackResult
			}
		}
		if parsed.Tool == "read_file" && inferSafeToolFromTask(task) == "list_files" {
			fallbackInput := coerceListFilesInput(json.RawMessage("{}"), task)
			fallbackResult, fallbackErr := o.runTool("list_files", fallbackInput)
			if fallbackErr == nil {
				return "🦦 " + fallbackResult
			}
		}
		if strings.Contains(strings.ToLower(err.Error()), "outside allowed directories") {
			return fmt.Sprintf("🦦 %v\n\n%s", err, o.accessGuidance())
		}
		return fmt.Sprintf("🦦 Tool error: %v", err)
	}

	if parsed.Tool == "move_file" && strings.HasPrefix(result, "Moved files:") {
		if err := o.recordMoveHistory(task, parsed.Input); err != nil {
			log.Printf("otter move history save failed: %v", err)
		}
	}

	return "🦦 " + result
}

func (o *Orchestrator) runTool(toolName string, input json.RawMessage) (string, error) {
	if err := permissions.ValidateToolCall(toolName, input); err != nil {
		return "", err
	}
	log.Printf("otter tool start: %s", toolName)
	result, err := o.registry.Execute(toolName, input)
	if err != nil {
		log.Printf("otter tool fail: %s err=%v", toolName, err)
		return "", err
	}
	log.Printf("otter tool success: %s", toolName)
	return result, nil
}

type moveHistoryInput struct {
	Source  string              `json:"source"`
	Target  string              `json:"target"`
	Moves   []map[string]string `json:"moves"`
	Confirm bool                `json:"confirm"`
}

func (o *Orchestrator) recordMoveHistory(task string, input json.RawMessage) error {
	var req moveHistoryInput
	if err := json.Unmarshal(input, &req); err != nil {
		return err
	}

	records := make([]settings.MoveRecord, 0, len(req.Moves)+1)
	if strings.TrimSpace(req.Source) != "" || strings.TrimSpace(req.Target) != "" {
		records = append(records, settings.MoveRecord{
			Source: strings.TrimSpace(req.Source),
			Target: strings.TrimSpace(req.Target),
		})
	}
	for _, move := range req.Moves {
		source := strings.TrimSpace(move["source"])
		target := strings.TrimSpace(move["target"])
		if source == "" || target == "" {
			continue
		}
		records = append(records, settings.MoveRecord{
			Source: source,
			Target: target,
		})
	}
	if len(records) == 0 {
		return nil
	}

	return settings.SaveMoveHistory(settings.MoveHistory{
		Task:      task,
		CreatedAt: time.Now(),
		Moves:     records,
	})
}

func (o *Orchestrator) handleUndoTask(task string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(task))
	if !isUndoRequest(lower) {
		return "", false
	}

	history, err := settings.LoadMoveHistory()
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't load the last move history: %v", err), true
	}
	if len(history.Moves) == 0 {
		return "🦦 I don't have a recent move to undo.", true
	}

	reversed := make([]map[string]string, 0, len(history.Moves))
	for index := len(history.Moves) - 1; index >= 0; index-- {
		move := history.Moves[index]
		reversed = append(reversed, map[string]string{
			"source": move.Target,
			"target": move.Source,
		})
	}

	input, _ := json.Marshal(map[string]any{
		"moves":   reversed,
		"confirm": true,
	})
	result, err := o.runTool("move_file", input)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't undo the last move: %v", err), true
	}
	if err := settings.ClearMoveHistory(); err != nil {
		log.Printf("otter move history clear failed: %v", err)
	}
	return "🦦 Undid the last move.\n" + result, true
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
	case strings.Contains(lower, "write"), strings.Contains(lower, "create"):
		return "write_file"
	case strings.Contains(lower, "move"), strings.Contains(lower, "organize"):
		return "move_file"
	default:
		return ""
	}
}

func isUndoRequest(taskLower string) bool {
	return taskLower == "undo" ||
		strings.Contains(taskLower, "undo last move") ||
		strings.Contains(taskLower, "undo organize") ||
		strings.Contains(taskLower, "undo that") ||
		strings.Contains(taskLower, "revert last move")
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
	clean = stripTaskFlags(clean)
	return clean
}

func stripTaskFlags(value string) string {
	clean := strings.TrimSpace(value)
	for {
		lower := strings.ToLower(clean)
		stripped := false
		for _, token := range []string{"confirm=true", "confirm=false", "overwrite=true", "overwrite=false", "confirm", "overwrite"} {
			index := strings.LastIndex(lower, token)
			if index < 0 {
				continue
			}
			if strings.TrimSpace(clean[index+len(token):]) != "" {
				continue
			}
			clean = strings.TrimSpace(clean[:index])
			clean = strings.TrimRight(clean, "?.!,;:")
			stripped = true
			break
		}
		if !stripped {
			return clean
		}
	}
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
		"- summarize note files\n" +
		"- write new files safely\n" +
		"- move files safely (with dry-run for batches)\n" +
		"- undo the last move\n\n" +
		"Examples:\n" +
		"- otter \"list files in ~/Downloads\"\n" +
		"- otter \"read ~/notes/today.md\"\n" +
		"- otter \"Read my latest notes over the last 10 days\"\n" +
		"- otter \"undo last move\"\n\n" +
		o.accessGuidance()
}

func (o *Orchestrator) handleComposedTask(task string) (string, bool) {
	trimmed := strings.TrimSpace(task)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "read ") && strings.Contains(lower, " then write ") {
		response := o.runReadAndWriteTask(trimmed)
		return response, true
	}
	return "", false
}

func (o *Orchestrator) runReadAndWriteTask(task string) string {
	sources, outputPath, err := parseReadThenWriteTask(task)
	if err != nil {
		return "🦦 " + err.Error()
	}

	contents := make(map[string]string, len(sources))
	for _, source := range sources {
		input, _ := json.Marshal(map[string]string{"path": source})
		readResult, runErr := o.runTool("read_file", input)
		if runErr != nil {
			if strings.Contains(strings.ToLower(runErr.Error()), "outside allowed directories") {
				return fmt.Sprintf("🦦 %v\n\n%s", runErr, o.accessGuidance())
			}
			return fmt.Sprintf("🦦 Couldn't read %s: %v", source, runErr)
		}
		contents[source] = stripReadPrefix(readResult)
	}

	report := synthesizeFromSources(contents)
	writeInput, _ := json.Marshal(map[string]any{
		"path":      outputPath,
		"content":   report,
		"overwrite": false,
	})
	writeResult, runErr := o.runTool("write_file", writeInput)
	if runErr != nil {
		if strings.Contains(strings.ToLower(runErr.Error()), "outside allowed directories") {
			return fmt.Sprintf("🦦 %v\n\n%s", runErr, o.accessGuidance())
		}
		return fmt.Sprintf("🦦 Couldn't write output file: %v", runErr)
	}

	writtenPath := extractWrittenPath(writeResult)
	return fmt.Sprintf("🦦 Wrote synthesized plan to %s\nSummary: used %d source files and created a new file without overwrite.", writtenPath, len(sources))
}

func parseReadThenWriteTask(task string) ([]string, string, error) {
	re := regexp.MustCompile(`(?i)^read\s+(.+?)\s+then write\s+.+?\s+to\s+(.+)$`)
	matches := re.FindStringSubmatch(strings.TrimSpace(task))
	if len(matches) != 3 {
		return nil, "", errors.New("I couldn't parse that request. Try: otter \"read <file1> and <file2>, then write a new report to <output-file>\"")
	}

	sourceChunk := strings.Trim(matches[1], " ,")
	output := sanitizeTaskPath(matches[2])
	output = strings.Trim(output, ",")
	if output == "" {
		return nil, "", errors.New("output path is required")
	}

	parts := strings.Split(sourceChunk, " and ")
	if len(parts) < 2 {
		return nil, "", errors.New("please provide at least two source files with 'and'")
	}
	sources := make([]string, 0, len(parts))
	for _, part := range parts {
		candidate := sanitizeTaskPath(part)
		candidate = strings.Trim(candidate, ",")
		if candidate == "" {
			continue
		}
		sources = append(sources, candidate)
	}
	if len(sources) < 2 {
		return nil, "", errors.New("please provide at least two valid source files")
	}
	return uniqueStrings(sources), output, nil
}

func synthesizeFromSources(contents map[string]string) string {
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	builder := strings.Builder{}
	builder.WriteString("# Otter Synthesized Plan\n\n")
	builder.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
	builder.WriteString("## Source files\n")
	for _, path := range paths {
		builder.WriteString("- " + path + "\n")
	}
	builder.WriteString("\n## Key points\n")
	for _, path := range paths {
		excerpt := firstNonEmptyLine(contents[path])
		if len(excerpt) > 140 {
			excerpt = excerpt[:140] + "..."
		}
		builder.WriteString(fmt.Sprintf("- %s: %s\n", filepath.Base(path), excerpt))
	}
	builder.WriteString("\n## Draft actions\n")
	builder.WriteString("- Align outreach priorities with the notes above.\n")
	builder.WriteString("- Prepare a concise message draft per contact.\n")
	builder.WriteString("- Track follow-ups and next decision date.\n")
	return builder.String()
}

func firstNonEmptyLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return "(no content)"
}

func stripReadPrefix(result string) string {
	if index := strings.Index(result, ":\n"); index >= 0 && index+2 < len(result) {
		return result[index+2:]
	}
	return result
}

func isOrganizeDownloadsRequest(taskLower string) bool {
	return strings.Contains(taskLower, "organize") && (strings.Contains(taskLower, "downloads") ||
		strings.Contains(taskLower, "subfolder") ||
		strings.Contains(taskLower, "folder") ||
		strings.Contains(taskLower, "music"))
}

func (o *Orchestrator) planOrganizeDownloads(task string) (ToolCall, bool) {
	previewOnly := strings.Contains(strings.ToLower(task), "preview") ||
		strings.Contains(strings.ToLower(task), "dry run") ||
		strings.Contains(strings.ToLower(task), "dry-run")

	sourceRoot, err := resolveOrganizeSourceRoot(task, o.allowedDirs)
	if err != nil {
		return ToolCall{Error: fmt.Sprintf("I couldn't resolve a source folder for that request: %v", err)}, true
	}

	targetFolder := extractOrganizeTargetFolder(task)
	musicOnly := strings.Contains(strings.ToLower(task), "music")

	moves, err := buildDownloadMoves(sourceRoot, targetFolder, musicOnly)
	if err != nil {
		return ToolCall{Error: fmt.Sprintf("I couldn't plan Download organization: %v", err)}, true
	}
	if len(moves) == 0 {
		return ToolCall{Error: "Downloads already looks organized. I found no files to move."}, true
	}

	input, _ := json.Marshal(map[string]any{
		"moves":   moves,
		"confirm": !previewOnly,
	})
	return ToolCall{Tool: "move_file", Input: input}, true
}

func resolveOrganizeSourceRoot(task string, allowedDirs []string) (string, error) {
	if pathValue := extractOrganizeSourcePath(task); pathValue != "" {
		resolved, err := tools.ResolvePath(pathValue)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}

	for _, candidate := range allowedDirs {
		if strings.EqualFold(filepath.Base(candidate), "Downloads") {
			return candidate, nil
		}
	}
	if len(allowedDirs) > 0 {
		return allowedDirs[0], nil
	}
	return "", fmt.Errorf("no allowed directories are configured")
}

func extractOrganizeSourcePath(task string) string {
	lower := strings.ToLower(task)
	for _, marker := range []string{" from ", " in ", " out of ", " under "} {
		index := strings.Index(lower, marker)
		if index < 0 {
			continue
		}
		remainder := strings.TrimSpace(task[index+len(marker):])
		remainder = strings.TrimSpace(strings.TrimSuffix(remainder, "."))
		remainder = strings.TrimSpace(strings.TrimSuffix(remainder, ","))
		if remainder == "" {
			continue
		}
		lowerRemainder := strings.ToLower(remainder)
		for _, stop := range []string{" into ", " to ", " called ", " named ", " subfolder ", " folder "} {
			if stopIndex := strings.Index(lowerRemainder, stop); stopIndex >= 0 {
				remainder = strings.TrimSpace(remainder[:stopIndex])
				lowerRemainder = strings.ToLower(remainder)
			}
		}
		clean := sanitizeTaskPath(remainder)
		if looksLikeOrganizeSource(clean) {
			return clean
		}
	}
	return ""
}

func looksLikeOrganizeSource(value string) bool {
	clean := strings.ToLower(strings.TrimSpace(value))
	if clean == "" {
		return false
	}
	if strings.HasPrefix(clean, "~/") || strings.HasPrefix(clean, "$home/") || strings.HasPrefix(clean, "/") {
		return true
	}
	if strings.Contains(clean, string(os.PathSeparator)) {
		return true
	}
	for _, token := range []string{"downloads", "desktop", "documents", "music", "pictures", "movies", "videos", "notes", "work", "projects"} {
		if clean == token || strings.HasPrefix(clean, token+" ") || strings.HasPrefix(clean, "my "+token) || strings.Contains(clean, "/"+token) {
			return true
		}
	}
	return false
}

func extractOrganizeTargetFolder(task string) string {
	lower := strings.ToLower(task)
	for _, marker := range []string{"subfolder called", "folder called", "subfolder named", "folder named"} {
		index := strings.Index(lower, marker)
		if index < 0 {
			continue
		}
		candidate := strings.TrimSpace(task[index+len(marker):])
		candidate = sanitizeTaskPath(candidate)
		candidate = strings.Trim(candidate, "\"'`")
		candidate = strings.Trim(candidate, ",.")
		if candidate != "" {
			return canonicalOrganizerFolderName(candidate)
		}
	}

	if strings.Contains(lower, "audio") {
		return "Audio"
	}
	return ""
}

func canonicalOrganizerFolderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "audio":
		return "Audio"
	case "documents":
		return "Documents"
	case "images":
		return "Images"
	case "archives":
		return "Archives"
	case "code":
		return "Code"
	case "video", "videos":
		return "Video"
	case "other":
		return "Other"
	default:
		return strings.TrimSpace(name)
	}
}

func buildDownloadMoves(rootPath, targetFolder string, musicOnly bool) ([]map[string]string, error) {
	moves := make([]map[string]string, 0, 32)
	categoryCache := make(map[string]string)
	err := filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, err error) error {
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
			if targetFolder != "" && strings.EqualFold(name, targetFolder) {
				return filepath.SkipDir
			}
			return nil
		}
		if musicOnly && !isMusicFile(name) {
			return nil
		}
		source := filepath.Clean(path)
		category := targetFolder
		if category == "" {
			category = categorizeDownloadFile(name)
		}
		if parentCategory, ok := uniformDirectoryCategory(filepath.Dir(source), rootPath, categoryCache); ok && parentCategory == category {
			relativePath, relErr := filepath.Rel(rootPath, source)
			if relErr == nil {
				moves = append(moves, map[string]string{
					"source": source,
					"target": filepath.Join(rootPath, category, relativePath),
				})
				return nil
			}
		}
		target := filepath.Join(rootPath, category, name)
		if source == target {
			return nil
		}
		moves = append(moves, map[string]string{
			"source": source,
			"target": target,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return moves, nil
}

func uniformDirectoryCategory(dirPath, rootPath string, cache map[string]string) (string, bool) {
	cleanDir := filepath.Clean(dirPath)
	cleanRoot := filepath.Clean(rootPath)
	if !strings.HasPrefix(cleanDir, cleanRoot) {
		return "", false
	}
	if category, ok := cache[cleanDir]; ok {
		return category, category != ""
	}

	entries, err := os.ReadDir(cleanDir)
	if err != nil {
		cache[cleanDir] = ""
		return "", false
	}

	category := ""
	hasVisibleFile := false
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			if strings.EqualFold(name, "Audio") {
				continue
			}
			childCategory, childOK := uniformDirectoryCategory(filepath.Join(cleanDir, name), cleanRoot, cache)
			if !childOK {
				cache[cleanDir] = ""
				return "", false
			}
			if category == "" {
				category = childCategory
			} else if category != childCategory {
				cache[cleanDir] = ""
				return "", false
			}
			continue
		}

		hasVisibleFile = true
		fileCategory := categorizeDownloadFile(name)
		if fileCategory == "" {
			cache[cleanDir] = ""
			return "", false
		}
		if category == "" {
			category = fileCategory
		} else if category != fileCategory {
			cache[cleanDir] = ""
			return "", false
		}
	}

	if !hasVisibleFile || category == "" {
		cache[cleanDir] = ""
		return "", false
	}

	cache[cleanDir] = category
	return category, true
}

func isMusicFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".m4a", ".wav", ".flac", ".aac", ".ogg", ".aiff":
		return true
	default:
		return false
	}
}

func categorizeDownloadFile(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic":
		return "Images"
	case ".pdf", ".doc", ".docx", ".txt", ".md", ".rtf", ".pages":
		return "Documents"
	case ".zip", ".rar", ".7z", ".tar", ".gz":
		return "Archives"
	case ".mp3", ".wav", ".m4a":
		return "Audio"
	case ".mp4", ".mov", ".mkv":
		return "Video"
	case ".go", ".js", ".ts", ".py", ".java", ".rb", ".sh":
		return "Code"
	default:
		return "Other"
	}
}

func (o *Orchestrator) summaryReportToolCall(task string) (ToolCall, error) {
	path, err := extractTargetPath(task, "to")
	if err != nil {
		return ToolCall{}, err
	}
	if strings.TrimSpace(path) == "" {
		return ToolCall{}, fmt.Errorf("please include a destination path")
	}
	paths, collectErr := collectRecentNotePaths(o.allowedDirs, 10, 8)
	if collectErr != nil {
		return ToolCall{}, fmt.Errorf("I couldn't scan notes: %w", collectErr)
	}
	if len(paths) == 0 {
		return ToolCall{}, fmt.Errorf("I couldn't find note files in allowed directories.\n\n%s", notesAccessGuidance())
	}
	content := o.buildSummaryReportContent(paths)
	target := buildReportTarget(path)
	input, _ := json.Marshal(map[string]any{
		"path":      target,
		"content":   content,
		"overwrite": false,
	})
	return ToolCall{Tool: "write_file", Input: input}, nil
}

func (o *Orchestrator) buildSummaryReportContent(paths []string) string {
	input, _ := json.Marshal(map[string]any{"paths": paths})
	result, err := o.runTool("summarize_files", input)
	if err != nil {
		return "Summary:\n- I couldn't summarize files automatically."
	}
	builder := strings.Builder{}
	builder.WriteString("# Otter Summary Report\n\n")
	builder.WriteString(fmt.Sprintf("Generated: %s\n\n", time.Now().Format(time.RFC3339)))
	builder.WriteString("## Source files\n")
	for _, path := range paths {
		builder.WriteString("- " + path + "\n")
	}
	builder.WriteString("\n## Summary\n")
	builder.WriteString(strings.TrimSpace(result))
	builder.WriteString("\n")
	return builder.String()
}

func extractTargetPath(task, marker string) (string, error) {
	lower := strings.ToLower(task)
	find := " " + strings.ToLower(marker) + " "
	index := strings.LastIndex(lower, find)
	if index < 0 {
		return "", fmt.Errorf("destination path is required")
	}
	path := strings.TrimSpace(task[index+len(find):])
	path = sanitizeTaskPath(path)
	if path == "" {
		return "", fmt.Errorf("destination path is required")
	}
	return path, nil
}

func buildReportTarget(path string) string {
	if strings.HasSuffix(strings.ToLower(path), ".md") || strings.HasSuffix(strings.ToLower(path), ".txt") {
		return path
	}
	filename := fmt.Sprintf("otter-summary-%s.md", time.Now().Format("20060102-150405"))
	return filepath.Join(path, filename)
}

func extractWrittenPath(result string) string {
	const prefix = "Wrote file: "
	if strings.HasPrefix(result, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(result, prefix))
	}
	return strings.TrimSpace(result)
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
