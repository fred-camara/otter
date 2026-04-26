package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"otter/internal/audit"
	"otter/internal/cleanup"
	"otter/internal/model"
	"otter/internal/organize"
	"otter/internal/permissions"
	"otter/internal/planner"
	"otter/internal/recovery"
	"otter/internal/settings"
	"otter/internal/tools"
)

const (
	defaultModelName           = "qwen2.5-coder:14b"
	defaultOllamaURL           = "http://127.0.0.1:11434"
	maxPlanRetries             = 2
	defaultModelSummaryTimeout = 120 * time.Second
)

type Orchestrator struct {
	registry            *tools.Registry
	planner             planner.Planner
	modelGen            model.Interface
	allowedDirs         []string
	modelName           string
	audit               *audit.Logger
	modelSummaryTimeout time.Duration
	pendingCleanup      *pendingCleanupContext
}

type pendingCleanupContext struct {
	Root                  string
	ReportPath            string
	DetectedEmptyCount    int
	ProtectedSkippedCount int
	Timestamp             time.Time
	SuggestedStageCommand string
}

func NewOrchestrator(allowedDirs []string, planner planner.Planner) (*Orchestrator, error) {
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
		registry:            tools.NewRegistry(listTool, readTool, summarizeTool, writeTool, moveTool),
		planner:             planner,
		allowedDirs:         normalizedDirs,
		modelSummaryTimeout: defaultModelSummaryTimeout,
	}, nil
}

func NewOrchestratorFromEnv() (*Orchestrator, error) {
	return NewOrchestratorForMode("cli")
}

func NewOrchestratorForMode(mode string) (*Orchestrator, error) {
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

	modelName := ""
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "chat":
		modelName, _ = ResolveChatModelName(cfg, strings.TrimSpace(os.Getenv("OTTER_MODEL")))
	default:
		modelName, _ = ResolvePlannerModelName(cfg, strings.TrimSpace(os.Getenv("OTTER_MODEL")))
	}
	ollamaURL := firstNonEmpty(strings.TrimSpace(os.Getenv("OTTER_OLLAMA_URL")), defaultOllamaURL)
	pl := planner.NewOllamaPlanner(model.NewOllama(modelName, ollamaURL))
	orch, err := NewOrchestrator(dirs, pl)
	if err != nil {
		return nil, err
	}
	orch.modelGen = model.NewOllamaText(modelName, ollamaURL)
	orch.modelName = modelName
	return orch, nil
}

func RunTask(task string) string {
	return RunTaskWithMode(task, "cli")
}

func RunTaskWithMode(task, mode string) string {
	orch, err := NewOrchestratorForMode(mode)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't initialize tools: %v", err)
	}
	return orch.RunWithMode(task, mode)
}

func (o *Orchestrator) Run(task string) string {
	return o.RunWithMode(task, "cli")
}

func (o *Orchestrator) RunWithMode(task, mode string) (output string) {
	o.audit = audit.Start(task, mode, firstNonEmpty(o.modelName, defaultModelName))
	defer func() {
		if o.audit != nil {
			o.audit.LogFinalOutput(output)
		}
		o.audit = nil
	}()

	if o.planner == nil {
		return fmt.Sprintf("🦦 Stub: I received your task: %q", task)
	}

	if response, handled := o.handleComposedTask(task); handled {
		return response
	}

	if response, handled := o.handleUndoTask(task); handled {
		return response
	}
	if response, handled := o.handleConversationalTask(task); handled {
		return response
	}

	if response, handled := o.handleSummarizeWithModelTask(task); handled {
		return response
	}

	if response, handled := o.handleRecoveryTask(task); handled {
		return response
	}
	if response, handled := o.handleCleanupEmptyFoldersTask(task); handled {
		return response
	}
	if response, handled := o.handleAudioOrganizeTask(task, mode); handled {
		return response
	}

	if directCall, ok := o.directToolCallForTask(task); ok {
		return o.executeToolCall(task, directCall)
	}

	toolNames, err := o.registry.Names()
	if err != nil {
		o.logAuditError("tool_registry", err)
		return fmt.Sprintf("🦦 Could not load tools: %v", err)
	}
	o.logPlannerRequest(planner.Request{
		Task:  task,
		Tools: toolNames,
	})

	var parsed ToolCall
	for attempt := 0; attempt <= maxPlanRetries; attempt++ {
		resp, err := o.planner.Plan(context.Background(), planner.Request{
			Task:  task,
			Tools: toolNames,
		})
		if err != nil {
			o.logAuditError("planner", err)
			return plannerErrorMessage(err)
		}
		o.logPlannerResponseRaw(attempt, resp.RawJSON)

		parsed, err = parseToolCall(resp.RawJSON)
		if err != nil {
			o.logAuditError("planner_parse", err)
			if attempt == maxPlanRetries {
				if response, handled := o.conversationalFallbackForPlannerFailure(task); handled {
					return response
				}
				return "🦦 I couldn't understand that request yet. Try rephrasing with a concrete action like `list files in ~/Downloads`."
			}
			continue
		}
		if strings.TrimSpace(parsed.Error) != "" {
			return "🦦 " + parsed.Error
		}
		o.logPlannerResponseParsed(parsed)
		parsed.Tool = normalizeToolName(parsed.Tool)
		if strings.TrimSpace(parsed.Tool) == "" {
			if attempt == maxPlanRetries {
				o.logAuditError("planner_parse", fmt.Errorf("planner did not select a tool"))
				if response, handled := o.conversationalFallbackForPlannerFailure(task); handled {
					return response
				}
				return "🦦 I couldn't understand that request yet. Try rephrasing with a concrete action like `list files in ~/Downloads`."
			}
			continue
		}
		break
	}

	return o.executeToolCall(task, parsed)
}

func (o *Orchestrator) RunOrganizeAudioCLI(root, contextRoot string, execute, deeperAnalysis bool, in io.Reader, out io.Writer) (string, error) {
	runTask := fmt.Sprintf("organize audio --root %s --context-root %s --execute=%t --deeper-analysis=%t", root, contextRoot, execute, deeperAnalysis)
	o.audit = audit.Start(runTask, "cli", firstNonEmpty(o.modelName, defaultModelName))
	defer func() {
		if o.audit != nil {
			o.audit = nil
		}
	}()
	org := organize.NewService(o.audit, o.modelGen)
	plan, err := org.GeneratePlan(organize.GeneratePlanRequest{
		Profile:        organize.ProfileAudio,
		Root:           root,
		ContextRoot:    contextRoot,
		RequestID:      o.audit.RunID(),
		DeeperAnalysis: deeperAnalysis,
	})
	if err != nil {
		o.audit.LogError("organize_audio_generate_plan", err)
		return "", err
	}
	summary := org.SummarizePlan(plan)
	if !execute {
		o.audit.LogFinalOutput("🦦 " + summary + "\n\nDry-run only. No files were moved.")
		return "🦦 " + summary + "\n\nDry-run only. No files were moved.", nil
	}

	reader := bufio.NewReader(in)
	fmt.Fprintln(out, summary)
	fmt.Fprint(out, "\nProceed with this plan? [y/N] ")
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		message := "🦦 Execution cancelled. No files were moved."
		o.audit.LogFinalOutput(message)
		return message, nil
	}

	result, err := org.ExecutePlan(organize.ExecutePlanRequest{
		Plan:     plan,
		Approved: true,
	})
	if err != nil {
		o.audit.LogError("organize_audio_execute", err)
		return "", err
	}
	lines := []string{
		"🦦 Audio organization executed safely.",
		fmt.Sprintf("Moved: %d", result.Executed),
		fmt.Sprintf("Skipped: %d", result.Skipped),
		fmt.Sprintf("Detected %d empty folders. Wrote report: %s", result.EmptyFolderCount, result.EmptyFolderReportPath),
		fmt.Sprintf("Plan log: %s", plan.PlanPath),
		fmt.Sprintf("Execution report: %s", result.ExecutionLogPath),
	}
	message := strings.Join(lines, "\n")
	o.audit.LogFinalOutput(message)
	return message, nil
}

func (o *Orchestrator) RunCleanupEmptyFoldersCLI(root string) (string, error) {
	runTask := fmt.Sprintf("cleanup empty-folders --root %s", root)
	o.audit = audit.Start(runTask, "cli", firstNonEmpty(o.modelName, defaultModelName))
	defer func() {
		if o.audit != nil {
			o.audit = nil
		}
	}()
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return "", err
	}
	if !pathWithinAllowed(resolvedRoot, o.allowedDirs) {
		return "", fmt.Errorf("cleanup root is outside allowed directories")
	}
	service := cleanup.NewService(o.audit)
	result, err := service.GenerateEmptyFoldersReport(cleanup.ReportRequest{
		Scopes: []cleanup.Scope{
			{Label: "Inside root", Root: resolvedRoot},
		},
	})
	if err != nil {
		return "", err
	}
	message := "🦦 " + cleanup.SummarizeReport(result) + fmt.Sprintf("\nDetected %d empty folders. Wrote report: %s", result.Total, result.ReportPath)
	o.pendingCleanup = &pendingCleanupContext{
		Root:                  resolvedRoot,
		ReportPath:            result.ReportPath,
		DetectedEmptyCount:    result.Total,
		ProtectedSkippedCount: result.ProtectedSkippedTotal,
		Timestamp:             time.Now().UTC(),
		SuggestedStageCommand: fmt.Sprintf("stage empty folders in %s yes", resolvedRoot),
	}
	o.audit.LogFinalOutput(message)
	return message, nil
}

func (o *Orchestrator) RunStageEmptyFoldersCLI(root, stageRoot string, confirm bool) (string, error) {
	runTask := fmt.Sprintf("cleanup empty-folders stage --root %s --stage-root %s", root, stageRoot)
	o.audit = audit.Start(runTask, "cli", firstNonEmpty(o.modelName, defaultModelName))
	defer func() {
		if o.audit != nil {
			o.audit = nil
		}
	}()
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return "", err
	}
	if !pathWithinAllowed(resolvedRoot, o.allowedDirs) {
		return "", fmt.Errorf(o.cleanupAccessDeniedMessage(resolvedRoot))
	}
	service := cleanup.NewService(o.audit)
	if !confirm {
		preview, previewErr := service.PreviewStageEmptyFolders(resolvedRoot)
		if previewErr != nil {
			return "", previewErr
		}
		return preview.Preview + "\n\nProceed? [y/N]\nRe-run with --confirm to stage.", nil
	}
	result, err := service.StageEmptyFolders(cleanup.StageRequest{
		Root:      resolvedRoot,
		StageRoot: stageRoot,
	})
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf("🦦 Moved %d empty folders into staging: %s\nDetected %d empty folders. Wrote report: %s", result.Moved, result.StageRoot, result.TotalFound, result.ReportPath)
	o.pendingCleanup = &pendingCleanupContext{
		Root:                  resolvedRoot,
		ReportPath:            result.ReportPath,
		DetectedEmptyCount:    result.TotalFound,
		ProtectedSkippedCount: result.ProtectedSkippedTotal,
		Timestamp:             time.Now().UTC(),
		SuggestedStageCommand: fmt.Sprintf("stage empty folders in %s yes", resolvedRoot),
	}
	o.audit.LogFinalOutput(message)
	return message, nil
}

func (o *Orchestrator) handleAudioOrganizeTask(task, mode string) (string, bool) {
	trimmed := strings.TrimSpace(task)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return "", false
	}

	org := organize.NewService(o.audit, o.modelGen)
	if mode == "chat" && (lower == "y" || lower == "yes") {
		pending, err := org.LoadPendingPlan()
		if err != nil {
			return "🦦 I don't have a pending audio plan to execute.", true
		}
		result, execErr := org.ExecutePlan(organize.ExecutePlanRequest{
			Plan:     pending.Plan,
			Approved: true,
		})
		if execErr != nil {
			return fmt.Sprintf("🦦 I couldn't execute the pending audio plan: %v", execErr), true
		}
		_ = org.ClearPendingPlan()
		return fmt.Sprintf("🦦 Audio organization executed.\nMoved: %d\nSkipped: %d\nDetected %d empty folders. Wrote report: %s\nExecution report: %s", result.Executed, result.Skipped, result.EmptyFolderCount, result.EmptyFolderReportPath, result.ExecutionLogPath), true
	}

	if !isAudioOrganizeRequest(lower) {
		return "", false
	}

	root, contextRoot, deeperAnalysis, err := extractAudioScopeFromTask(trimmed)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't parse audio organize scope: %v", err), true
	}
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't resolve organize root: %v", err), true
	}
	if !pathWithinAllowed(resolvedRoot, o.allowedDirs) {
		return fmt.Sprintf("🦦 organize root is outside allowed directories\n\n%s", o.accessGuidance()), true
	}

	plan, err := org.GeneratePlan(organize.GeneratePlanRequest{
		Profile:        organize.ProfileAudio,
		Root:           root,
		ContextRoot:    contextRoot,
		RequestID:      o.audit.RunID(),
		DeeperAnalysis: deeperAnalysis,
	})
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't generate audio organization plan: %v", err), true
	}
	summary := org.SummarizePlan(plan)

	executeRequested := strings.Contains(lower, "execute")
	explicitApprove := hasExplicitYes(lower)
	if !executeRequested {
		_ = org.SavePendingPlan(plan)
		return "🦦 " + summary + "\n\nProceed with this plan? [y/N]\nReply `yes` to execute.", true
	}
	if !explicitApprove {
		_ = org.SavePendingPlan(plan)
		return "🦦 " + summary + "\n\nProceed with this plan? [y/N]\nReply `yes` to execute.", true
	}

	result, err := org.ExecutePlan(organize.ExecutePlanRequest{
		Plan:     plan,
		Approved: true,
	})
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't execute the audio organization plan: %v", err), true
	}
	_ = org.ClearPendingPlan()
	return fmt.Sprintf("🦦 Audio organization executed.\nMoved: %d\nSkipped: %d\nDetected %d empty folders. Wrote report: %s\nExecution report: %s", result.Executed, result.Skipped, result.EmptyFolderCount, result.EmptyFolderReportPath, result.ExecutionLogPath), true
}

func (o *Orchestrator) handleCleanupEmptyFoldersTask(task string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(task))
	if !isCleanupEmptyFoldersRequest(lower) {
		return "", false
	}
	root, err := extractCleanupRootFromTask(task, o.pendingCleanup)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't parse cleanup scope: %v", err), true
	}
	resolvedRoot, err := tools.ResolvePath(root)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't resolve cleanup root: %v", err), true
	}
	if !pathWithinAllowed(resolvedRoot, o.allowedDirs) {
		return "🦦 " + o.cleanupAccessDeniedMessage(resolvedRoot), true
	}
	service := cleanup.NewService(o.audit)
	if wantsStageEmptyFolders(lower) {
		if !hasExplicitYes(lower) {
			preview, previewErr := service.PreviewStageEmptyFolders(resolvedRoot)
			if previewErr != nil {
				return fmt.Sprintf("🦦 I couldn't prepare empty-folder staging preview: %v", previewErr), true
			}
			return "🦦 " + preview.Preview + "\n\nProceed? [y/N]\nReply with the same request plus `yes` to confirm staging.", true
		}
		stageResult, stageErr := service.StageEmptyFolders(cleanup.StageRequest{
			Root: resolvedRoot,
		})
		if stageErr != nil {
			return fmt.Sprintf("🦦 I couldn't stage empty folders: %v", stageErr), true
		}
		o.pendingCleanup = &pendingCleanupContext{
			Root:                  resolvedRoot,
			ReportPath:            stageResult.ReportPath,
			DetectedEmptyCount:    stageResult.TotalFound,
			ProtectedSkippedCount: stageResult.ProtectedSkippedTotal,
			Timestamp:             time.Now().UTC(),
			SuggestedStageCommand: fmt.Sprintf("stage empty folders in %s yes", resolvedRoot),
		}
		return fmt.Sprintf("🦦 Moved %d empty folders into staging: %s\nDetected %d empty folders. Wrote report: %s", stageResult.Moved, stageResult.StageRoot, stageResult.TotalFound, stageResult.ReportPath), true
	}

	result, err := service.GenerateEmptyFoldersReport(cleanup.ReportRequest{
		Scopes: []cleanup.Scope{{Label: "Inside root", Root: resolvedRoot}},
	})
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't generate empty-folder report: %v", err), true
	}
	o.pendingCleanup = &pendingCleanupContext{
		Root:                  resolvedRoot,
		ReportPath:            result.ReportPath,
		DetectedEmptyCount:    result.Total,
		ProtectedSkippedCount: result.ProtectedSkippedTotal,
		Timestamp:             time.Now().UTC(),
		SuggestedStageCommand: fmt.Sprintf("stage empty folders in %s yes", resolvedRoot),
	}
	return "🦦 " + cleanup.SummarizeReport(result) + fmt.Sprintf("\nDetected %d empty folders. Wrote report:\n%s\nI can stage them into a review folder with:\n`%s`", result.Total, result.ReportPath, o.pendingCleanup.SuggestedStageCommand), true
}

func isAudioOrganizeRequest(taskLower string) bool {
	if strings.Contains(taskLower, "organize audio") {
		return true
	}
	if strings.Contains(taskLower, "organize my audio") {
		return true
	}
	if strings.Contains(taskLower, "audio folder") {
		return true
	}
	if strings.Contains(taskLower, "audio organization") {
		return true
	}
	if strings.Contains(taskLower, "separate mp3") || strings.Contains(taskLower, "separate wav") {
		return true
	}
	return false
}

func isCleanupEmptyFoldersRequest(taskLower string) bool {
	if strings.Contains(taskLower, "cleanup empty-folders") ||
		strings.Contains(taskLower, "clean empty-folders") ||
		strings.Contains(taskLower, "report empty-folders") {
		return true
	}
	if refersToPreviousCleanup(taskLower) {
		return true
	}
	if strings.Contains(taskLower, "empty folder") || strings.Contains(taskLower, "empty folders") {
		return strings.Contains(taskLower, "find") ||
			strings.Contains(taskLower, "list") ||
			strings.Contains(taskLower, "report") ||
			strings.Contains(taskLower, "show") ||
			strings.Contains(taskLower, "cleanup") ||
			strings.Contains(taskLower, "stage") ||
			strings.Contains(taskLower, "move")
	}
	return false
}

func wantsStageEmptyFolders(taskLower string) bool {
	if refersToPreviousCleanup(taskLower) {
		return strings.Contains(taskLower, "stage") || strings.Contains(taskLower, "move")
	}
	return (strings.Contains(taskLower, "empty folder") || strings.Contains(taskLower, "empty folders")) &&
		(strings.Contains(taskLower, "stage") || strings.Contains(taskLower, "move"))
}

func hasExplicitYes(taskLower string) bool {
	parts := strings.Fields(taskLower)
	for _, part := range parts {
		token := strings.Trim(part, ".,!?;:")
		if token == "y" || token == "yes" {
			return true
		}
	}
	return false
}

func extractAudioScopeFromTask(task string) (string, string, bool, error) {
	root := "~/Downloads/audio"
	contextRoot := "~/Downloads"
	deeper := false
	lowerTask := strings.ToLower(task)
	if strings.Contains(lowerTask, "deeper analysis") || strings.Contains(lowerTask, "deper analysis") {
		deeper = true
	}
	parts := strings.Fields(task)
	for i := 0; i < len(parts); i++ {
		token := strings.TrimSpace(parts[i])
		if token == "--root" && i+1 < len(parts) {
			root = sanitizeTaskPath(parts[i+1])
			i++
			continue
		}
		if token == "--context-root" && i+1 < len(parts) {
			contextRoot = sanitizeTaskPath(parts[i+1])
			i++
			continue
		}
		if token == "--deper-analysis" || token == "--deeper-analysis" {
			deeper = true
			continue
		}
		lower := strings.ToLower(token)
		if lower == "deeper-analysis" || lower == "deep-analysis" {
			deeper = true
		}
	}
	return root, contextRoot, deeper, nil
}

func extractCleanupRootFromTask(task string, pending *pendingCleanupContext) (string, error) {
	root := ""
	parts := strings.Fields(task)
	for i := 0; i < len(parts); i++ {
		token := strings.TrimSpace(parts[i])
		if token == "--root" && i+1 < len(parts) {
			root = sanitizeTaskPath(parts[i+1])
			i++
			continue
		}
	}
	lower := strings.ToLower(task)
	for _, marker := range []string{" in ", " under ", " inside "} {
		if idx := strings.LastIndex(lower, marker); idx >= 0 {
			candidate := sanitizeCleanupPathSegment(task[idx+len(marker):])
			if strings.TrimSpace(candidate) != "" {
				root = normalizeNamedFolderRoot(candidate)
			}
			break
		}
	}
	if strings.TrimSpace(root) == "" {
		if pending != nil && refersToPreviousCleanup(lower) {
			return pending.Root, nil
		}
		if strings.Contains(lower, "downloads") {
			return "~/Downloads", nil
		}
		if strings.Contains(lower, "audio folder") || strings.Contains(lower, "audio") {
			home, _ := os.UserHomeDir()
			audioPath := filepath.Join(home, "Downloads", "audio")
			if info, err := os.Stat(audioPath); err == nil && info.IsDir() {
				return audioPath, nil
			}
			return "~/Downloads/audio", nil
		}
		if pending != nil {
			return pending.Root, nil
		}
		return "~/Downloads", nil
	}
	return normalizeNamedFolderRoot(root), nil
}

func refersToPreviousCleanup(lowerTask string) bool {
	trimmed := strings.TrimSpace(lowerTask)
	if trimmed == "stage them" || trimmed == "stage them yes" || trimmed == "yes stage them" {
		return true
	}
	if strings.Contains(lowerTask, "stage them") ||
		strings.Contains(lowerTask, "stage those") ||
		strings.Contains(lowerTask, "stage empty folders yes") ||
		strings.Contains(lowerTask, "move them to review") ||
		strings.Contains(lowerTask, "the same folder") {
		return true
	}
	if strings.Contains(lowerTask, "there") && strings.Contains(lowerTask, "stage") {
		return true
	}
	return false
}

func sanitizeCleanupPathSegment(value string) string {
	candidate := sanitizeTaskPath(value)
	for _, stop := range []string{" yes", " confirm", " please", " now", " and ", " but "} {
		lower := strings.ToLower(candidate)
		if idx := strings.Index(lower, stop); idx >= 0 {
			candidate = strings.TrimSpace(candidate[:idx])
		}
	}
	return strings.TrimSpace(candidate)
}

func normalizeNamedFolderRoot(value string) string {
	clean := strings.TrimSpace(value)
	lower := strings.ToLower(clean)
	switch lower {
	case "downloads", "my downloads":
		return "~/Downloads"
	case "documents", "my documents":
		return "~/Documents"
	case "desktop", "my desktop":
		return "~/Desktop"
	case "audio", "my audio folder":
		return "~/Downloads/audio"
	default:
		return clean
	}
}

func (o *Orchestrator) cleanupAccessDeniedMessage(attempted string) string {
	lines := []string{
		"I tried to stage or report empty folders under:",
		attempted,
		"",
		"But Otter currently has access only to:",
	}
	if len(o.allowedDirs) == 0 {
		lines = append(lines, "- (no allowed directories configured)")
	} else {
		for _, dir := range o.allowedDirs {
			lines = append(lines, "- "+dir)
		}
	}
	lines = append(lines, "",
		"You can grant access with:",
		`otter "allow access to ~/Downloads"`,
	)
	return strings.Join(lines, "\n")
}

func (o *Orchestrator) conversationalFallbackForPlannerFailure(task string) (string, bool) {
	if isLikelyConversationalInput(task) {
		return "🦦 " + conversationalResponse(task), true
	}
	return "", false
}

func (o *Orchestrator) handleConversationalTask(task string) (string, bool) {
	trimmed := strings.TrimSpace(task)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return "", false
	}
	// If a message mixes a greeting with an actionable request
	// (e.g. "hey can you list files..."), prefer task execution.
	if isLikelyToolRequest(lower) {
		return "", false
	}
	if !isLikelyConversationalInput(trimmed) {
		return "", false
	}
	return "🦦 " + conversationalResponse(trimmed), true
}

func isLikelyConversationalInput(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	if lower == "" {
		return false
	}
	for _, phrase := range []string{
		"hello", "hi", "hey", "good morning", "good afternoon", "good evening",
		"how are you", "what are you", "who are you", "thanks", "thank you",
		"nice to meet you", "what can you do", "help me",
	} {
		if lower == phrase || strings.HasPrefix(lower, phrase+" ") || strings.Contains(lower, phrase+"?") {
			return true
		}
	}
	if strings.HasSuffix(lower, "?") && !isLikelyToolRequest(lower) {
		return true
	}
	return false
}

func isLikelyToolRequest(lowerTask string) bool {
	toolSignals := []string{
		"list", "files", "read", "summar", "write", "move", "organize", "recover",
		"undo", "access", "directory", "folder", "path", "report", "run", "model",
	}
	for _, signal := range toolSignals {
		if strings.Contains(lowerTask, signal) {
			return true
		}
	}
	return false
}

func conversationalResponse(task string) string {
	lower := strings.ToLower(strings.TrimSpace(task))
	if strings.Contains(lower, "hello") || strings.HasPrefix(lower, "hi") || strings.HasPrefix(lower, "hey") {
		return "Hello. I can summarize files, organize or recover folders, and manage access. Try `/help` in chat or ask `list files in ~/Downloads`."
	}
	if strings.Contains(lower, "thank") {
		return "You're welcome."
	}
	if strings.Contains(lower, "help") || strings.Contains(lower, "what can you do") {
		return "I can summarize files, organize/recover files, create plans, manage directory access, inspect runs, and change models."
	}
	return "I can help with local file operations and planning. Try a concrete request like `summarize this file: ~/Downloads/file.pdf`."
}

func plannerErrorMessage(err error) string {
	base := fmt.Sprintf("🦦 Planner error: %v", err)
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "127.0.0.1:11434") ||
		strings.Contains(lower, "call ollama") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "no such host") {
		return base + "\n\nTo enable chat/planning safely:\n" +
			"1. Start Ollama locally (`ollama serve`).\n" +
			"2. Ensure the model exists (`ollama pull qwen2.5-coder:14b` or set `OTTER_MODEL`).\n" +
			"3. Verify `OTTER_OLLAMA_URL` (default `http://127.0.0.1:11434`)."
	}
	return base
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
			pathValue = normalizePathAlias(candidate)
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
	if call, ok := o.summarizeToolCallFromTask(trimmed); ok {
		return call, true
	}

	if isListFilesRequest(lowered) {
		pathValue := "."
		if strings.Contains(lowered, " in ") {
			index := strings.LastIndex(lowered, " in ")
			candidate := strings.TrimSpace(trimmed[index+4:])
			if candidate != "" {
				pathValue = normalizePathAlias(candidate)
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

func (o *Orchestrator) summarizeToolCallFromTask(task string) (ToolCall, bool) {
	lower := strings.ToLower(strings.TrimSpace(task))
	if !strings.Contains(lower, "summar") {
		return ToolCall{}, false
	}

	var rawPaths []string
	trimmed := strings.TrimSpace(task)

	if strings.Contains(lower, "summarize this file:") || strings.Contains(lower, "summarise this file:") {
		if index := strings.Index(trimmed, ":"); index >= 0 && index+1 < len(trimmed) {
			rawPaths = append(rawPaths, strings.TrimSpace(trimmed[index+1:]))
		}
	} else if strings.HasPrefix(lower, "summarize files ") || strings.HasPrefix(lower, "summarise files ") {
		chunk := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "summarize files "), "summarise files "))
		for _, part := range strings.Split(chunk, " and ") {
			for _, item := range strings.Split(part, ",") {
				candidate := sanitizeTaskPath(item)
				if candidate != "" {
					rawPaths = append(rawPaths, candidate)
				}
			}
		}
	} else if strings.HasPrefix(lower, "summarize ") || strings.HasPrefix(lower, "summarise ") {
		chunk := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "summarize "), "summarise "))
		chunk = strings.TrimSpace(stripLeadingSummarizeFileTargetPrefix(chunk))
		if chunk != "" && (strings.Contains(chunk, "/") || strings.Contains(chunk, "~") || strings.Contains(chunk, ".")) {
			rawPaths = append(rawPaths, sanitizeTaskPath(chunk))
		}
	}

	paths := uniqueStrings(rawPaths)
	if len(paths) == 0 {
		return ToolCall{}, false
	}
	resolved, err := o.resolveSummarizePaths(paths)
	if err != nil {
		return ToolCall{Error: err.Error()}, true
	}

	input, _ := json.Marshal(map[string][]string{"paths": resolved})
	return ToolCall{Tool: "summarize_files", Input: input}, true
}

func stripLeadingSummarizeFileTargetPrefix(value string) string {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	prefixes := []string{
		"this file:",
		"this file ",
		"the file:",
		"the file ",
		"file:",
		"file ",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return trimmed
}

func (o *Orchestrator) handleSummarizeWithModelTask(task string) (string, bool) {
	call, ok := o.summarizeToolCallFromTask(task)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(call.Error) != "" {
		return "🦦 " + call.Error, true
	}
	if call.Tool != "summarize_files" {
		return "", false
	}

	var payload struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(call.Input, &payload); err != nil || len(payload.Paths) == 0 {
		return o.executeToolCall(task, call), true
	}
	if o.modelGen == nil {
		return o.executeToolCall(task, call), true
	}

	contextByPath := make(map[string]string, len(payload.Paths))
	for _, path := range payload.Paths {
		text, err := tools.ExtractSummarizableText(path, o.allowedDirs)
		if err != nil {
			return fmt.Sprintf("🦦 Tool error: %v", err), true
		}
		if len(text) > 12000 {
			text = text[:12000] + "\n...[truncated]"
		}
		contextByPath[path] = buildModelSummaryContext(path, text)
	}

	output, err := o.generateModelSummaryWithTimeout(buildModelSummaryPrompt(task, contextByPath))
	if err != nil {
		o.logAuditError("model_summary", err)
		fallback := o.executeToolCall(task, call)
		return fmt.Sprintf("🦦 Model summary unavailable: %v\nUsing tool-based fallback.\n\n%s", err, fallback), true
	}
	output = strings.TrimSpace(output)
	if output == "" {
		fallback := o.executeToolCall(task, call)
		return "🦦 Model summary unavailable: model returned empty output.\nUsing tool-based fallback.\n\n" + fallback, true
	}
	return "🦦 " + output, true
}

func (o *Orchestrator) generateModelSummaryWithTimeout(prompt string) (string, error) {
	if o.modelGen == nil {
		return "", fmt.Errorf("model not configured")
	}
	timeout := o.modelSummaryTimeout
	if timeout <= 0 {
		timeout = defaultModelSummaryTimeout
	}

	type result struct {
		output string
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		output, err := o.modelGen.Generate(prompt)
		ch <- result{output: output, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.output, res.err
	case <-timer.C:
		return "", fmt.Errorf("timed out after %s", timeout)
	}
}

func buildModelSummaryPrompt(task string, contextByPath map[string]string) string {
	paths := make([]string, 0, len(contextByPath))
	for path := range contextByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	builder := strings.Builder{}
	builder.WriteString("You are Otter. Summarize the provided document for the user.\n")
	builder.WriteString("Return concise markdown with:\n")
	builder.WriteString("- one short overview paragraph\n")
	builder.WriteString("- key facts\n")
	builder.WriteString("- optional concerns or missing info\n\n")
	builder.WriteString("User request: ")
	builder.WriteString(task)
	builder.WriteString("\n\n")
	builder.WriteString("Document contexts:\n")
	for _, path := range paths {
		builder.WriteString("### ")
		builder.WriteString(path)
		builder.WriteString("\n")
		builder.WriteString(contextByPath[path])
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func buildModelSummaryContext(path, text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "No extractable text."
	}

	hint := detectDocumentTypeHint(trimmed)

	lines := strings.Split(trimmed, "\n")
	previewLines := 8
	if len(lines) < previewLines {
		previewLines = len(lines)
	}
	excerpt := strings.Join(lines[:previewLines], "\n")
	if len(excerpt) > 2400 {
		excerpt = excerpt[:2400] + "\n...[truncated]"
	}

	builder := strings.Builder{}
	if hint != "" {
		builder.WriteString("Document type hint: ")
		builder.WriteString(hint)
		builder.WriteString("\n")
	}
	builder.WriteString("Extracted text preview for ")
	builder.WriteString(filepath.Base(path))
	builder.WriteString(":\n")
	builder.WriteString(excerpt)
	builder.WriteString("\n\n")
	builder.WriteString("Full extracted text length: ")
	builder.WriteString(fmt.Sprintf("%d chars", len(trimmed)))
	return builder.String()
}

func detectDocumentTypeHint(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "payslip") || strings.Contains(lower, "pay slip") || strings.Contains(lower, "gross pay") || strings.Contains(lower, "net pay"):
		return "payslip. Extract employer, pay period, gross pay, net pay, tax, and any totals."
	case strings.Contains(lower, "invoice") || strings.Contains(lower, "amount due") || strings.Contains(lower, "subtotal"):
		return "invoice. Extract vendor, invoice date, amount due, subtotal, tax, and total."
	case strings.Contains(lower, "receipt") || strings.Contains(lower, "total") || strings.Contains(lower, "tax"):
		return "receipt. Extract merchant, date, line items, tax, and total."
	default:
		return ""
	}
}

func (o *Orchestrator) resolveSummarizePaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, item := range paths {
		candidate := strings.TrimSpace(item)
		if candidate == "" {
			continue
		}
		if looksPathLike(candidate) {
			out = append(out, candidate)
			continue
		}

		matches, err := o.findFileByNameInAllowedDirs(candidate, 5)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			out = append(out, candidate)
			continue
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("I found multiple files named %q in allowed directories. Please provide a path.\n- %s", candidate, strings.Join(matches, "\n- "))
		}
		out = append(out, matches[0])
	}
	return out, nil
}

func (o *Orchestrator) findFileByNameInAllowedDirs(name string, limit int) ([]string, error) {
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	matches := make([]string, 0, 2)
	for _, root := range o.allowedDirs {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			base := d.Name()
			if strings.HasPrefix(base, ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if strings.EqualFold(base, "recovery_plan.json") || strings.EqualFold(base, "recovery_plan.md") {
				return nil
			}
			if strings.ToLower(base) == target {
				matches = append(matches, filepath.Clean(path))
				if len(matches) >= limit {
					return fmt.Errorf("match limit reached")
				}
			}
			return nil
		})
		if err != nil && !strings.Contains(err.Error(), "match limit reached") {
			return nil, err
		}
	}
	sort.Strings(matches)
	return uniqueStrings(matches), nil
}

func looksPathLike(value string) bool {
	clean := strings.TrimSpace(value)
	return strings.Contains(clean, "/") ||
		strings.Contains(clean, string(os.PathSeparator)) ||
		strings.HasPrefix(clean, "~") ||
		strings.HasPrefix(strings.ToLower(clean), "$home/")
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
		o.logAuditError("permissions", err)
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
		o.logAuditError("tool_execute", err)
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
		o.logAuditError("permissions", err)
		return "", err
	}
	log.Printf("otter tool start: %s", toolName)
	result, err := o.registry.Execute(toolName, input)
	o.logToolCall(toolName, input, result, err)
	if err != nil {
		log.Printf("otter tool fail: %s err=%v", toolName, err)
		return "", err
	}
	log.Printf("otter tool success: %s", toolName)
	return result, nil
}

func (o *Orchestrator) logPlannerRequest(req planner.Request) {
	if o.audit != nil {
		o.audit.LogPlannerRequest(req)
	}
}

func (o *Orchestrator) logPlannerResponseRaw(attempt int, raw string) {
	if o.audit != nil {
		o.audit.LogPlannerResponseRaw(attempt, raw)
	}
}

func (o *Orchestrator) logPlannerResponseParsed(parsed ToolCall) {
	if o.audit != nil {
		o.audit.LogPlannerResponseParsed(parsed)
	}
}

func (o *Orchestrator) logToolCall(toolName string, input json.RawMessage, result string, err error) {
	if o.audit != nil {
		o.audit.LogToolCall(toolName, input, result, err)
	}
}

func (o *Orchestrator) logAuditError(stage string, err error) {
	if o.audit != nil {
		o.audit.LogError(stage, err)
	}
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

func isRecoveryRequest(taskLower string) bool {
	if strings.Contains(taskLower, "recovery plan") {
		return true
	}
	if !strings.Contains(taskLower, "recover") {
		return false
	}
	return strings.Contains(taskLower, "file") ||
		strings.Contains(taskLower, "folder") ||
		strings.Contains(taskLower, "structure") ||
		strings.Contains(taskLower, "downloads") ||
		strings.Contains(taskLower, "cymatics")
}

func (o *Orchestrator) handleRecoveryTask(task string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(task))
	if !isRecoveryRequest(lower) {
		return "", false
	}

	root, err := o.resolveRecoveryRoot(task)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't resolve recovery scope: %v", err), true
	}
	if !pathWithinAllowed(root, o.allowedDirs) {
		return fmt.Sprintf("🦦 %s\n\n%s", "recovery root is outside allowed directories", o.accessGuidance()), true
	}

	logs := recovery.ParseLogEntries(task)
	if len(logs) == 0 {
		if history, historyErr := settings.LoadMoveHistory(); historyErr == nil {
			for _, move := range history.Moves {
				logs = append(logs, recovery.LogEntry{Source: move.Source, Target: move.Target})
			}
		}
	}
	logs = filterRecoveryLogsForRoot(logs, root)

	plan, err := recovery.Generate(root, logs)
	if err != nil {
		return fmt.Sprintf("🦦 I couldn't generate a recovery plan: %v", err), true
	}

	if err := o.writeRecoveryPlan(plan); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "outside allowed directories") {
			return fmt.Sprintf("🦦 %v\n\n%s", err, o.accessGuidance()), true
		}
		return fmt.Sprintf("🦦 I generated the plan but couldn't save plan files: %v", err), true
	}

	return fmt.Sprintf("🦦 Recovery plan created (dry-run only).\n- JSON: %s\n- Markdown: %s\n- Entries: %d (needs review: %d)\nNo file moves were executed.",
		filepath.Join(root, "recovery_plan.json"),
		filepath.Join(root, "recovery_plan.md"),
		len(plan.Entries),
		plan.NeedsReview,
	), true
}

func (o *Orchestrator) resolveRecoveryRoot(task string) (string, error) {
	if candidate := extractRecoveryPath(task); candidate != "" {
		resolved, err := tools.ResolvePath(candidate)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("recovery target must be a directory")
		}
		return resolved, nil
	}

	lower := strings.ToLower(task)
	if strings.Contains(lower, "cymatics") {
		for _, dir := range o.allowedDirs {
			if strings.EqualFold(filepath.Base(dir), "Downloads") {
				return dir, nil
			}
		}
	}
	if len(o.allowedDirs) == 0 {
		return "", fmt.Errorf("no allowed directories are configured")
	}
	return o.allowedDirs[0], nil
}

func extractRecoveryPath(task string) string {
	lower := strings.ToLower(task)
	for _, marker := range []string{" in ", " for ", " under "} {
		index := strings.LastIndex(lower, marker)
		if index < 0 {
			continue
		}
		candidate := strings.TrimSpace(task[index+len(marker):])
		for _, stop := range []string{";", " using ", " with ", " log ", " logs ", " where "} {
			if stopIndex := strings.Index(strings.ToLower(candidate), stop); stopIndex >= 0 {
				candidate = strings.TrimSpace(candidate[:stopIndex])
			}
		}
		candidate = sanitizeTaskPath(candidate)
		if candidate == "" {
			continue
		}
		if looksLikeOrganizeSource(candidate) {
			return candidate
		}
	}
	return ""
}

func pathWithinAllowed(path string, allowedDirs []string) bool {
	clean := filepath.Clean(path)
	for _, allowed := range allowedDirs {
		if clean == allowed {
			return true
		}
		if strings.HasPrefix(clean, allowed+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func filterRecoveryLogsForRoot(logs []recovery.LogEntry, root string) []recovery.LogEntry {
	filtered := make([]recovery.LogEntry, 0, len(logs))
	for _, entry := range logs {
		source, sourceErr := tools.ResolvePath(entry.Source)
		target, targetErr := tools.ResolvePath(entry.Target)
		if sourceErr != nil || targetErr != nil {
			continue
		}
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) && target != root {
			continue
		}
		filtered = append(filtered, recovery.LogEntry{
			Source: source,
			Target: target,
		})
	}
	return filtered
}

func (o *Orchestrator) writeRecoveryPlan(plan recovery.Plan) error {
	jsonText, err := recovery.PlanJSON(plan)
	if err != nil {
		return err
	}
	markdown := recovery.PlanMarkdown(plan)

	jsonPath := filepath.Join(plan.RootPath, "recovery_plan.json")
	mdPath := filepath.Join(plan.RootPath, "recovery_plan.md")

	if err := o.writeTextFileWithConfirm(jsonPath, jsonText); err != nil {
		return err
	}
	if err := o.writeTextFileWithConfirm(mdPath, markdown); err != nil {
		return err
	}
	return nil
}

func (o *Orchestrator) writeTextFileWithConfirm(path, content string) error {
	input, err := json.Marshal(map[string]any{
		"path":      path,
		"content":   content,
		"overwrite": true,
		"confirm":   true,
	})
	if err != nil {
		return err
	}
	_, err = o.runTool("write_file", input)
	return err
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

func normalizePathAlias(value string) string {
	clean := sanitizeTaskPath(value)
	switch strings.ToLower(strings.TrimSpace(clean)) {
	case "downloads", "my downloads", "the downloads":
		return "~/Downloads"
	case "downlaods", "my downlaods", "the downlaods":
		return "~/Downloads"
	case "dowloads", "my dowloads", "the dowloads":
		return "~/Downloads"
	case "desktop", "desktops", "my desktop", "my desktops", "the desktop", "the desktops":
		return "~/Desktop"
	case "documents", "my documents", "the documents":
		return "~/Documents"
	case "music", "my music":
		return "~/Music"
	case "pictures", "my pictures", "photos", "my photos":
		return "~/Pictures"
	case "movies", "my movies", "videos", "my videos":
		return "~/Movies"
	case "notes", "my notes":
		return "~/notes"
	default:
		return clean
	}
}

func isListFilesRequest(taskLower string) bool {
	if strings.HasPrefix(taskLower, "list files") || strings.HasPrefix(taskLower, "list the files") {
		return true
	}
	if strings.HasPrefix(taskLower, "show files") || strings.HasPrefix(taskLower, "show me files") {
		return true
	}
	// Allow conversational prefixes/suffixes around the core intent, e.g.
	// "cool now list the files in downloads".
	if strings.Contains(taskLower, "list files") || strings.Contains(taskLower, "list the files") {
		return true
	}
	if strings.Contains(taskLower, "show files") || strings.Contains(taskLower, "show me files") {
		return true
	}
	return false
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
	if paths := parseComposedListFilesPaths(trimmed); len(paths) >= 2 {
		return o.runComposedListFilesTask(paths), true
	}
	if paths := parseReadThenSummarizePaths(trimmed); len(paths) > 0 {
		return o.runReadThenSummarizeTask(paths), true
	}
	if strings.HasPrefix(lower, "read ") && strings.Contains(lower, " then write ") {
		response := o.runReadAndWriteTask(trimmed)
		return response, true
	}
	return "", false
}

func parseComposedListFilesPaths(task string) []string {
	if strings.TrimSpace(task) == "" {
		return nil
	}
	listPrefixRe := regexp.MustCompile(`(?i)(?:list|show(?:\s+me)?)\s+(?:the\s+)?files\s+in\s+`)
	loc := listPrefixRe.FindStringIndex(task)
	if len(loc) != 2 {
		return nil
	}
	remainder := strings.TrimSpace(task[loc[1]:])
	if remainder == "" {
		return nil
	}
	connectorRe := regexp.MustCompile(`(?i)\s+(?:and then|then|and)\s+`)
	parts := connectorRe.Split(remainder, -1)
	if len(parts) < 2 {
		return nil
	}
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		candidatePart := strings.TrimSpace(part)
		if candidatePart == "" {
			continue
		}
		candidatePart = listPrefixRe.ReplaceAllString(candidatePart, "")
		candidate := normalizePathAlias(candidatePart)
		if candidate == "" {
			continue
		}
		paths = append(paths, candidate)
	}
	unique := uniqueStrings(paths)
	if len(unique) < 2 {
		return nil
	}
	return unique
}

func (o *Orchestrator) runComposedListFilesTask(paths []string) string {
	results := make([]string, 0, len(paths))
	for _, pathValue := range paths {
		input, _ := json.Marshal(map[string]string{"path": pathValue})
		result, err := o.runTool("list_files", input)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "outside allowed directories") {
				return fmt.Sprintf("🦦 %v\n\n%s", err, o.accessGuidance())
			}
			return fmt.Sprintf("🦦 Tool error: %v", err)
		}
		results = append(results, result)
	}
	return "🦦 " + strings.Join(results, "\n\n")
}

func parseReadThenSummarizePaths(task string) []string {
	re := regexp.MustCompile(`(?i)\bread\s+(.+?)\s+(?:and then|then)\s+summar(?:ize|ise)\b`)
	matches := re.FindStringSubmatch(strings.TrimSpace(task))
	if len(matches) < 2 {
		return nil
	}
	chunk := strings.TrimSpace(matches[1])
	if chunk == "" {
		return nil
	}

	candidates := make([]string, 0, 4)
	for _, part := range strings.Split(chunk, " and ") {
		for _, item := range strings.Split(part, ",") {
			candidate := sanitizeTaskPath(item)
			candidate = strings.TrimSpace(strings.Trim(candidate, ","))
			if candidate != "" {
				candidates = append(candidates, candidate)
			}
		}
	}
	candidates = uniqueStrings(candidates)
	if len(candidates) == 0 {
		return nil
	}

	explicitPathLike := false
	for _, candidate := range candidates {
		if looksPathLike(candidate) || strings.Contains(filepath.Base(candidate), ".") {
			explicitPathLike = true
			break
		}
	}
	if !explicitPathLike {
		return nil
	}
	return candidates
}

func (o *Orchestrator) runReadThenSummarizeTask(paths []string) string {
	resolved, err := o.resolveSummarizePaths(paths)
	if err != nil {
		return "🦦 " + err.Error()
	}
	input, _ := json.Marshal(map[string][]string{"paths": resolved})
	result, runErr := o.runTool("summarize_files", input)
	if runErr != nil {
		if strings.Contains(strings.ToLower(runErr.Error()), "outside allowed directories") {
			return fmt.Sprintf("🦦 %v\n\n%s", runErr, o.accessGuidance())
		}
		return fmt.Sprintf("🦦 Tool error: %v", runErr)
	}
	return "🦦 " + result
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
