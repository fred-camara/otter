package audit

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"otter/internal/settings"
)

const (
	auditRunsDirEnv = "OTTER_AUDIT_RUNS_DIR"
	auditDisableEnv = "OTTER_AUDIT_DISABLED"
)

type Logger struct {
	mu       sync.Mutex
	runID    string
	runDir   string
	enabled  bool
	meta     Metadata
	hasError bool
}

type Metadata struct {
	RunID     string `json:"run_id"`
	Timestamp string `json:"timestamp"`
	Mode      string `json:"mode"`
	Model     string `json:"model"`
	CWD       string `json:"cwd"`
	Status    string `json:"status"`
}

type ErrorEntry struct {
	Timestamp string `json:"timestamp"`
	Stage     string `json:"stage"`
	Message   string `json:"message"`
}

type ToolCallEntry struct {
	Timestamp string `json:"timestamp"`
	Tool      string `json:"tool"`
	Input     any    `json:"input,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

type RunSummary struct {
	ID       string
	Dir      string
	Input    string
	Status   string
	Errors   []string
	Modified time.Time
}

func Start(task, mode, model string) *Logger {
	if isAuditDisabled() {
		return &Logger{enabled: false}
	}
	runsDir, err := RunsDir()
	if err != nil {
		return &Logger{enabled: false}
	}

	now := time.Now().UTC()
	runID := buildRunID(now)
	dirName := fmt.Sprintf("%s-%s", now.Format("20060102-150405.000"), slugifyTask(task))
	runDir := filepath.Join(runsDir, dirName)
	for index := 2; ; index++ {
		_, statErr := os.Stat(runDir)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return &Logger{enabled: false}
		}
		runDir = filepath.Join(runsDir, fmt.Sprintf("%s-%d", dirName, index))
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return &Logger{enabled: false}
	}

	logger := &Logger{runID: runID, runDir: runDir, enabled: true}
	logger.writeFile("input.txt", redact(task)+"\n")
	cwd, _ := os.Getwd()
	meta := Metadata{
		RunID:     runID,
		Timestamp: now.Format(time.RFC3339Nano),
		Mode:      strings.TrimSpace(mode),
		Model:     redact(strings.TrimSpace(model)),
		CWD:       redact(strings.TrimSpace(cwd)),
		Status:    "running",
	}
	logger.meta = meta
	logger.writeJSON("metadata.json", meta)
	return logger
}

func (l *Logger) RunID() string {
	if l == nil {
		return ""
	}
	return l.runID
}

func (l *Logger) RunDir() string {
	if l == nil {
		return ""
	}
	return l.runDir
}

func (l *Logger) LogPlannerRequest(value any) {
	if l == nil || !l.enabled {
		return
	}
	l.writeJSON("planner_request.json", sanitizeAny(value))
}

func (l *Logger) LogPlannerResponseRaw(attempt int, raw string) {
	if l == nil || !l.enabled {
		return
	}
	header := fmt.Sprintf("--- attempt %d @ %s ---\n", attempt, time.Now().UTC().Format(time.RFC3339Nano))
	l.appendFile("planner_response_raw.txt", header+redact(truncate(raw, 20000))+"\n")
}

func (l *Logger) LogPlannerResponseParsed(value any) {
	if l == nil || !l.enabled {
		return
	}
	l.writeJSON("planner_response_parsed.json", sanitizeAny(value))
}

func (l *Logger) LogToolCall(tool string, input []byte, result string, err error) {
	if l == nil || !l.enabled {
		return
	}
	entry := ToolCallEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Tool:      redact(tool),
		Result:    redact(truncate(result, 3000)),
	}
	if len(input) > 0 {
		entry.Input = sanitizeToolInput(input)
	}
	if err != nil {
		entry.Error = redact(truncate(err.Error(), 3000))
	}
	l.appendJSONL("tool_calls.jsonl", entry)
}

func (l *Logger) LogError(stage string, err error) {
	if l == nil || !l.enabled || err == nil {
		return
	}
	entry := ErrorEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Stage:     redact(stage),
		Message:   redact(truncate(err.Error(), 4000)),
	}
	l.appendJSONL("errors.jsonl", entry)
	l.mu.Lock()
	l.hasError = true
	l.mu.Unlock()
}

func (l *Logger) LogFinalOutput(value string) {
	if l == nil || !l.enabled {
		return
	}
	l.writeFile("final_output.md", redact(value)+"\n")
	l.mu.Lock()
	if l.meta.RunID == "" {
		l.mu.Unlock()
		return
	}
	if l.hasError {
		l.meta.Status = "failure"
	} else {
		l.meta.Status = "success"
	}
	meta := l.meta
	l.mu.Unlock()
	l.writeJSON("metadata.json", meta)
}

func RunsDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv(auditRunsDirEnv)); override != "" {
		return override, nil
	}
	configPath, err := settings.ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(configPath), "runs"), nil
}

func isAuditDisabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(auditDisableEnv)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func ListRunSummaries(limit int) ([]RunSummary, error) {
	runsDir, err := RunsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []RunSummary{}, nil
		}
		return nil, err
	}
	summaries := make([]RunSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDir := filepath.Join(runsDir, entry.Name())
		metaBytes, _ := os.ReadFile(filepath.Join(runDir, "metadata.json"))
		inputBytes, _ := os.ReadFile(filepath.Join(runDir, "input.txt"))
		errorBytes, _ := os.ReadFile(filepath.Join(runDir, "errors.jsonl"))
		var meta Metadata
		_ = json.Unmarshal(metaBytes, &meta)
		status := "success"
		errorsOut := make([]string, 0, 4)
		if len(errorBytes) > 0 {
			status = "failure"
			scanner := bufio.NewScanner(strings.NewReader(string(errorBytes)))
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				var item ErrorEntry
				if err := json.Unmarshal([]byte(line), &item); err == nil && item.Message != "" {
					errorsOut = append(errorsOut, item.Message)
				} else {
					errorsOut = append(errorsOut, line)
				}
				if len(errorsOut) >= 3 {
					break
				}
			}
		}
		info, _ := os.Stat(runDir)
		modified := time.Time{}
		if info != nil {
			modified = info.ModTime()
		}
		summaries = append(summaries, RunSummary{
			ID:       firstNonEmpty(meta.RunID, entry.Name()),
			Dir:      runDir,
			Input:    strings.TrimSpace(redact(truncate(string(inputBytes), 300))),
			Status:   status,
			Errors:   errorsOut,
			Modified: modified,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Modified.After(summaries[j].Modified)
	})
	if limit > 0 && len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries, nil
}

func ResolveRunDirectory(selector string) (string, error) {
	summaries, err := ListRunSummaries(0)
	if err != nil {
		return "", err
	}
	if len(summaries) == 0 {
		return "", fmt.Errorf("no runs found")
	}
	sel := strings.TrimSpace(strings.ToLower(selector))
	if sel == "" || sel == "latest" {
		return summaries[0].Dir, nil
	}
	for _, item := range summaries {
		base := filepath.Base(item.Dir)
		if strings.EqualFold(item.ID, selector) || strings.EqualFold(base, selector) || strings.HasPrefix(strings.ToLower(item.ID), sel) || strings.HasPrefix(strings.ToLower(base), sel) {
			return item.Dir, nil
		}
	}
	return "", fmt.Errorf("run not found: %s", selector)
}

func (l *Logger) writeJSON(name string, value any) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	l.writeFile(name, string(payload)+"\n")
}

func (l *Logger) appendJSONL(name string, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	l.appendFile(name, string(payload)+"\n")
}

func (l *Logger) writeFile(name, content string) {
	if l == nil || !l.enabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.runDir, name)
	_ = os.WriteFile(path, []byte(content), 0o644)
}

func (l *Logger) appendFile(name, content string) {
	if l == nil || !l.enabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.runDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

func buildRunID(now time.Time) string {
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	return fmt.Sprintf("run_%s_%s", now.Format("20060102T150405.000"), hex.EncodeToString(random))
}

func slugifyTask(task string) string {
	text := strings.TrimSpace(strings.ToLower(task))
	if text == "" {
		return "empty-task"
	}
	re := regexp.MustCompile(`[^a-z0-9]+`)
	text = re.ReplaceAllString(text, "-")
	text = strings.Trim(text, "-")
	if text == "" {
		text = "task"
	}
	if len(text) > 32 {
		text = text[:32]
	}
	return text
}

func sanitizeToolInput(input []byte) any {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return redact(truncate(trimmed, 2000))
	}
	return sanitizeAny(value)
}

func sanitizeAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for k, v := range typed {
			lower := strings.ToLower(strings.TrimSpace(k))
			if lower == "authorization" || lower == "token" || strings.Contains(lower, "api_key") || lower == "apikey" || lower == "content" {
				clean[k] = "[redacted]"
				continue
			}
			clean[k] = sanitizeAny(v)
		}
		return clean
	case []any:
		out := make([]any, 0, len(typed))
		for _, v := range typed {
			out = append(out, sanitizeAny(v))
		}
		return out
	case string:
		return redact(truncate(typed, 2000))
	default:
		return value
	}
}

func redact(value string) string {
	if value == "" {
		return value
	}
	out := value
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)bearer\s+[a-z0-9\-\._~\+/]+=*`),
		regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)([^\s\"']+)`),
		regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*)([^\s\"']+)`),
		regexp.MustCompile(`(?i)(token\s*[:=]\s*)([^\s\"']+)`),
	}
	out = patterns[0].ReplaceAllString(out, "Bearer [REDACTED]")
	for _, re := range patterns[1:] {
		out = re.ReplaceAllString(out, "$1[REDACTED]")
	}
	return out
}

func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max] + "...[truncated]"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
