package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"otter/internal/model"
	"otter/internal/permissions"
	"otter/internal/tools"
)

const (
	defaultModelName = "qwen2.5-coder:14b"
	defaultOllamaURL = "http://127.0.0.1:11434"
	maxPlanRetries   = 2
)

type Orchestrator struct {
	registry *tools.Registry
	planner  Planner
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
		registry: tools.NewRegistry(listTool, readTool, summarizeTool),
		planner:  planner,
	}, nil
}

func NewOrchestratorFromEnv() (*Orchestrator, error) {
	dirs := parseAllowedDirs(os.Getenv("OTTER_ALLOWED_DIRS"))
	if len(dirs) == 0 {
		fallbackDirs, err := defaultAllowedDirs()
		if err != nil {
			return nil, err
		}
		dirs = fallbackDirs
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

	if directCall, ok := directToolCallForTask(task); ok {
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
	case "cat", "read", "readfile", "read_file_tool":
		return "read_file"
	case "summary", "summarize", "summarise", "summarizefiles", "summarize_file":
		return "summarize_files"
	}
	return normalized
}

func coerceListFilesInput(raw json.RawMessage, task string) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" && trimmed != "{}" {
		return raw
	}

	pathValue := "."
	lowered := strings.ToLower(task)
	if index := strings.LastIndex(lowered, " in "); index >= 0 {
		candidate := strings.TrimSpace(task[index+4:])
		if candidate != "" {
			pathValue = candidate
		}
	}

	payload, err := json.Marshal(map[string]string{"path": pathValue})
	if err != nil {
		return raw
	}
	return payload
}

func directToolCallForTask(task string) (ToolCall, bool) {
	trimmed := strings.TrimSpace(task)
	lowered := strings.ToLower(trimmed)

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
	if err := permissions.Validate(parsed.Tool); err != nil {
		return fmt.Sprintf("🦦 Permission denied: %v", err)
	}

	if parsed.Tool == "list_files" {
		parsed.Input = coerceListFilesInput(parsed.Input, task)
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
		return fmt.Sprintf("🦦 Tool error: %v", err)
	}

	return "🦦 " + result
}
