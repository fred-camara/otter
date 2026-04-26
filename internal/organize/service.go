package organize

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"otter/internal/audit"
	"otter/internal/cleanup"
	"otter/internal/model"
	"otter/internal/settings"
	"otter/internal/tools"
)

type Service struct {
	audit                     *audit.Logger
	now                       func() time.Time
	previewLimit              int
	ambiguityThreshold        float64
	maxAmbiguousFilesForModel int
	modelBatchSize            int
	maxModelPlanning          time.Duration
	modelCallTimeout          time.Duration
	engines                   map[string]profileEngine
}

type profileEngine struct {
	rules      DomainRules
	classifier ClassificationStrategy
}

type GeneratePlanRequest struct {
	Profile        string
	Root           string
	ContextRoot    string
	RequestID      string
	DeeperAnalysis bool
}

type Plan struct {
	PlanID      string        `json:"plan_id"`
	RequestID   string        `json:"request_id"`
	Profile     string        `json:"profile"`
	Root        string        `json:"root"`
	ContextRoot string        `json:"context_root"`
	CreatedAt   string        `json:"created_at"`
	PlanPath    string        `json:"plan_path"`
	ReportPath  string        `json:"report_path"`
	Stats       PlanningStats `json:"stats"`
	Progress    []string      `json:"progress"`
	Actions     []PlanAction  `json:"actions"`
}

type PlanningStats struct {
	ScannedFiles                        int    `json:"scanned_files"`
	RuleClassifiedFiles                 int    `json:"rule_classified_files"`
	QueuedAmbiguousFiles                int    `json:"queued_ambiguous_files"`
	ModelCandidateFiles                 int    `json:"model_candidate_files"`
	ModelClassifiedFiles                int    `json:"model_classified_files"`
	ReusedCachedClassifications         int    `json:"reused_cached_classifications"`
	ReviewByCapFiles                    int    `json:"review_by_cap_files"`
	ReviewByBudgetFiles                 int    `json:"review_by_budget_files"`
	ReviewByModelFailureFiles           int    `json:"review_by_model_failure_files"`
	ModelBatchesTotal                   int    `json:"model_batches_total"`
	ModelBatchesCompleted               int    `json:"model_batches_completed"`
	ModelBatchTimeouts                  int    `json:"model_batch_timeouts"`
	ModelBatchFailures                  int    `json:"model_batch_failures"`
	ModelBudgetExceeded                 bool   `json:"model_budget_exceeded"`
	StrategySource                      string `json:"strategy_source"`
	FallbackAmbiguousNotModelClassified int    `json:"fallback_ambiguous_not_model_classified"`
}

type PlanAction struct {
	PlanID          string   `json:"plan_id"`
	SourcePath      string   `json:"source_path"`
	DestinationPath string   `json:"destination_path"`
	Classification  string   `json:"classification"`
	DecisionSource  string   `json:"decision_source"`
	Confidence      float64  `json:"confidence"`
	ConfidenceBand  string   `json:"confidence_band"`
	RequiresReview  bool     `json:"requires_review"`
	Evidence        []string `json:"evidence"`
	SourceSizeBytes int64    `json:"source_size_bytes"`
	SourceModTime   string   `json:"source_mod_time"`
}

type ExecutePlanRequest struct {
	Plan     Plan
	Approved bool
}

type ExecutePlanResult struct {
	PlanID                string
	Executed              int
	Skipped               int
	MovedPaths            []string
	ExecutionLogPath      string
	EmptyFolderCount      int
	EmptyFolderReportPath string
}

type PendingPlan struct {
	SavedAt string `json:"saved_at"`
	Plan    Plan   `json:"plan"`
}

type planEvent struct {
	Timestamp      string   `json:"timestamp"`
	Event          string   `json:"event"`
	PlanID         string   `json:"plan_id"`
	RequestID      string   `json:"request_id"`
	Source         string   `json:"source,omitempty"`
	Destination    string   `json:"destination,omitempty"`
	Classification string   `json:"classification,omitempty"`
	DecisionSource string   `json:"decision_source,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	Status         string   `json:"status"`
	Evidence       []string `json:"evidence,omitempty"`
}

type cachedDecision struct {
	Classification string   `json:"classification"`
	DecisionSource string   `json:"decision_source"`
	Confidence     float64  `json:"confidence"`
	Evidence       []string `json:"evidence"`
}

type modelCacheFile struct {
	Entries map[string]cachedDecision `json:"entries"`
}

type ambiguousItem struct {
	input ClassificationInput
	info  os.FileInfo
	base  ClassificationDecision
}

func NewService(a *audit.Logger, modelGen ...model.Interface) *Service {
	var classifierModel model.Interface
	if len(modelGen) > 0 {
		classifierModel = modelGen[0]
	}
	audioRules := NewAudioRules()
	return &Service{
		audit:                     a,
		now:                       time.Now,
		previewLimit:              6,
		ambiguityThreshold:        0.75,
		maxAmbiguousFilesForModel: 200,
		modelBatchSize:            25,
		maxModelPlanning:          60 * time.Second,
		modelCallTimeout:          8 * time.Second,
		engines: map[string]profileEngine{
			ProfileAudio: {
				rules:      audioRules,
				classifier: NewAudioHybridClassifier(classifierModel),
			},
		},
	}
}

func (s *Service) GeneratePlan(req GeneratePlanRequest) (Plan, error) {
	profile := strings.TrimSpace(req.Profile)
	if profile == "" {
		profile = ProfileAudio
	}
	engine, ok := s.engines[profile]
	if !ok {
		return Plan{}, fmt.Errorf("unsupported organize profile: %s", profile)
	}

	rootInput := firstNonEmpty(req.Root, engine.rules.DefaultRoot())
	contextInput := firstNonEmpty(req.ContextRoot, engine.rules.DefaultContextRoot())
	root, err := tools.ResolvePath(rootInput)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve root: %w", err)
	}
	contextRoot, err := tools.ResolvePath(contextInput)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve context root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return Plan{}, fmt.Errorf("stat root: %w", err)
	}
	if !rootInfo.IsDir() {
		return Plan{}, fmt.Errorf("root must be a directory: %s", root)
	}
	context, err := engine.rules.PrepareContext(root, contextRoot)
	if err != nil {
		return Plan{}, fmt.Errorf("prepare context: %w", err)
	}

	ts := s.now().UTC()
	planID := fmt.Sprintf("%s-%s", profile, ts.Format("20060102-150405"))
	reportDir, err := cleanup.ReportDirForRoot(root)
	if err != nil {
		return Plan{}, err
	}
	planPath := filepath.Join(reportDir, fmt.Sprintf("%s_plan_%s.jsonl", profile, ts.Format("20060102-150405")))
	reportPath := filepath.Join(reportDir, fmt.Sprintf("%s_report_%s.md", profile, ts.Format("20060102-150405")))

	actions, stats, progress, err := s.buildActions(root, planID, engine, context, req.DeeperAnalysis)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		PlanID:      planID,
		RequestID:   strings.TrimSpace(req.RequestID),
		Profile:     profile,
		Root:        root,
		ContextRoot: contextRoot,
		CreatedAt:   ts.Format(time.RFC3339Nano),
		PlanPath:    planPath,
		ReportPath:  reportPath,
		Stats:       stats,
		Progress:    progress,
		Actions:     actions,
	}
	if err := s.writePlanJSONL(plan); err != nil {
		return Plan{}, err
	}
	if err := s.writePlanReport(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (s *Service) buildActions(root, planID string, engine profileEngine, context map[string]any, deeper bool) ([]PlanAction, PlanningStats, []string, error) {
	stats := PlanningStats{StrategySource: "rule-default"}
	progress := make([]string, 0, 12)
	emit := func(msg string) {
		progress = append(progress, msg)
		if s.audit != nil {
			s.audit.LogToolCall("organize_progress", nil, msg, nil)
		}
	}

	actions := make([]PlanAction, 0, 256)
	reserved := make(map[string]struct{}, 512)
	modelQueue := make([]ambiguousItem, 0, 256)
	maxAmbiguous := s.maxAmbiguousFilesForModel
	maxPlanning := s.maxModelPlanning
	if deeper {
		maxAmbiguous = 1000
		maxPlanning = 180 * time.Second
	}

	cache, cachePath, _ := loadModelCache()
	cacheDirty := false

	emit("scanning files")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		stats.ScannedFiles++
		if !engine.rules.IsCandidateFile(path, entry) {
			return nil
		}

		sourcePath := filepath.Clean(path)
		info, statErr := os.Stat(sourcePath)
		if statErr != nil {
			return nil
		}
		input := ClassificationInput{SourcePath: sourcePath, Root: root, Context: context}
		ruleDecision := normalizeDecision(engine.classifier.Classify(input))
		stats.RuleClassifiedFiles++

		if ruleDecision.Confidence >= s.ambiguityThreshold {
			actions = append(actions, s.makeAction(root, planID, sourcePath, info, engine, ruleDecision, reserved))
			return nil
		}

		stats.QueuedAmbiguousFiles++
		key := cacheKeyFor(sourcePath, info)
		if cached, ok := cache[key]; ok {
			cachedDecision := normalizeDecision(ClassificationDecision{
				Classification: cached.Classification,
				DecisionSource: firstNonEmpty(cached.DecisionSource, "hybrid"),
				Confidence:     cached.Confidence,
				RequiresReview: strings.HasPrefix(cached.Classification, "review/"),
				Evidence:       append(uniqueNonEmpty(cached.Evidence), "cache hit"),
			})
			actions = append(actions, s.makeAction(root, planID, sourcePath, info, engine, cachedDecision, reserved))
			stats.ReusedCachedClassifications++
			return nil
		}

		if len(modelQueue) < maxAmbiguous {
			modelQueue = append(modelQueue, ambiguousItem{input: input, info: info, base: ruleDecision})
			return nil
		}

		stats.ReviewByCapFiles++
		stats.FallbackAmbiguousNotModelClassified++
		decision := normalizeDecision(ClassificationDecision{
			Classification: "review/ambiguous",
			DecisionSource: "rule",
			Confidence:     0.40,
			RequiresReview: true,
			Evidence: append(ruleDecision.Evidence,
				"ambiguous model cap reached",
				fmt.Sprintf("max_ambiguous_files_for_model=%d", maxAmbiguous),
			),
		})
		actions = append(actions, s.makeAction(root, planID, sourcePath, info, engine, decision, reserved))
		return nil
	})
	if err != nil {
		return nil, stats, progress, err
	}
	emit(fmt.Sprintf("scanned %d files", stats.ScannedFiles))
	emit(fmt.Sprintf("rule-classified %d files", stats.RuleClassifiedFiles))
	emit(fmt.Sprintf("queued %d ambiguous files", len(modelQueue)))

	stats.ModelCandidateFiles = len(modelQueue)
	if len(modelQueue) > 0 {
		spec := OrganizationSpec{Source: "rule-default", Notes: []string{"conservative review routing when uncertain"}}
		if inferer, ok := engine.classifier.(StrategyInferer); ok {
			sample := buildStrategySample(modelQueue, context)
			inferred, inferErr := inferer.InferOrganizationSpec(sample, minDuration(s.modelCallTimeout, 6*time.Second))
			if inferErr == nil {
				spec = inferred
				stats.StrategySource = firstNonEmpty(inferred.Source, "model")
			} else {
				stats.StrategySource = "rule-default"
				emit("strategy inference fallback to rule-default")
			}
		}

		start := s.now()
		batchClassifier, ok := engine.classifier.(AmbiguousBatchClassifier)
		if !ok {
			ok = false
		}

		batchSize := s.modelBatchSize
		if batchSize < 25 {
			batchSize = 25
		}
		if batchSize > 50 {
			batchSize = 50
		}

		totalBatches := (len(modelQueue) + batchSize - 1) / batchSize
		stats.ModelBatchesTotal = totalBatches
		for batchIdx, offset := 0, 0; offset < len(modelQueue); batchIdx, offset = batchIdx+1, offset+batchSize {
			if s.now().Sub(start) >= maxPlanning {
				stats.ModelBudgetExceeded = true
				remaining := modelQueue[offset:]
				stats.ReviewByBudgetFiles += len(remaining)
				stats.FallbackAmbiguousNotModelClassified += len(remaining)
				emit(fmt.Sprintf("model budget exceeded; routing %d files to review", len(remaining)))
				for _, item := range remaining {
					decision := normalizeDecision(ClassificationDecision{
						Classification: "review/low_confidence",
						DecisionSource: "hybrid",
						Confidence:     0.35,
						RequiresReview: true,
						Evidence: append(item.base.Evidence,
							"time budget exceeded",
							fmt.Sprintf("max_model_planning_seconds=%d", int(maxPlanning.Seconds())),
						),
					})
					actions = append(actions, s.makeAction(root, planID, item.input.SourcePath, item.info, engine, decision, reserved))
				}
				break
			}

			end := offset + batchSize
			if end > len(modelQueue) {
				end = len(modelQueue)
			}
			batch := modelQueue[offset:end]
			inputs := make([]ClassificationInput, 0, len(batch))
			for _, item := range batch {
				inputs = append(inputs, item.input)
			}
			emit(fmt.Sprintf("sending batch %d/%d to model", batchIdx+1, totalBatches))

			if !ok {
				stats.ModelBatchFailures++
				stats.ReviewByModelFailureFiles += len(batch)
				stats.FallbackAmbiguousNotModelClassified += len(batch)
				for _, item := range batch {
					decision := normalizeDecision(ClassificationDecision{
						Classification: "review/ambiguous",
						DecisionSource: "rule",
						Confidence:     0.40,
						RequiresReview: true,
						Evidence: append(item.base.Evidence,
							"model batch classifier unavailable",
							"fallback to review",
						),
					})
					actions = append(actions, s.makeAction(root, planID, item.input.SourcePath, item.info, engine, decision, reserved))
				}
				continue
			}

			decisions, modelErr := batchClassifier.ClassifyAmbiguousBatch(inputs, spec, s.modelCallTimeout)
			if modelErr != nil || len(decisions) != len(batch) {
				if modelErr != nil && strings.Contains(strings.ToLower(modelErr.Error()), "timeout") {
					stats.ModelBatchTimeouts++
					emit(fmt.Sprintf("model timeout; routed batch %d to review", batchIdx+1))
				} else {
					stats.ModelBatchFailures++
					emit(fmt.Sprintf("model batch failure; routed batch %d to review", batchIdx+1))
				}
				stats.ReviewByModelFailureFiles += len(batch)
				stats.FallbackAmbiguousNotModelClassified += len(batch)
				for _, item := range batch {
					decision := normalizeDecision(ClassificationDecision{
						Classification: "review/ambiguous",
						DecisionSource: "hybrid",
						Confidence:     0.40,
						RequiresReview: true,
						Evidence: append(item.base.Evidence,
							"model batch failed or timed out",
							"fallback to review",
						),
					})
					actions = append(actions, s.makeAction(root, planID, item.input.SourcePath, item.info, engine, decision, reserved))
				}
				continue
			}

			stats.ModelBatchesCompleted++
			for idx, decision := range decisions {
				item := batch[idx]
				decision = normalizeDecision(decision)
				if decision.Confidence < s.ambiguityThreshold && !strings.HasPrefix(decision.Classification, "review/") {
					decision.Classification = "review/low_confidence"
					decision.RequiresReview = true
					decision.Evidence = append(decision.Evidence, "model confidence below threshold")
				}
				if strings.HasPrefix(decision.Classification, "review/") {
					decision.RequiresReview = true
				}
				actions = append(actions, s.makeAction(root, planID, item.input.SourcePath, item.info, engine, decision, reserved))
				stats.ModelClassifiedFiles++
				cache[cacheKeyFor(item.input.SourcePath, item.info)] = cachedDecision{
					Classification: decision.Classification,
					DecisionSource: decision.DecisionSource,
					Confidence:     decision.Confidence,
					Evidence:       decision.Evidence,
				}
				cacheDirty = true
			}
		}
	}

	if cacheDirty {
		_ = saveModelCache(cachePath, cache)
	}

	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Classification == actions[j].Classification {
			return actions[i].SourcePath < actions[j].SourcePath
		}
		return actions[i].Classification < actions[j].Classification
	})
	filtered := actions[:0]
	for _, action := range actions {
		if strings.TrimSpace(action.SourcePath) == "" {
			continue
		}
		filtered = append(filtered, action)
	}
	emit("wrote plan/report")
	return filtered, stats, progress, nil
}

func buildStrategySample(items []ambiguousItem, context map[string]any) StrategySample {
	extCount := make(map[string]int, 8)
	examples := make([]string, 0, 20)
	for _, item := range items {
		ext := strings.ToLower(filepath.Ext(item.input.SourcePath))
		extCount[ext]++
		if len(examples) < 20 {
			examples = append(examples, filepath.Base(item.input.SourcePath))
		}
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(extCount))
	for k, v := range extCount {
		pairs = append(pairs, kv{k: k, v: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v == pairs[j].v {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v > pairs[j].v
	})
	exts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		exts = append(exts, fmt.Sprintf("%s(%d)", pair.k, pair.v))
	}
	if len(exts) > 10 {
		exts = exts[:10]
	}
	return StrategySample{
		ContextFolders: stringSliceContext(context, "context_folders"),
		Examples:       examples,
		TopExtensions:  exts,
	}
}

func (s *Service) makeAction(root, planID, sourcePath string, info os.FileInfo, engine profileEngine, decision ClassificationDecision, reserved map[string]struct{}) PlanAction {
	destPath := engine.rules.BuildDestination(root, sourcePath, decision)
	destPath = uniqueDestination(destPath, reserved)
	if filepath.Clean(destPath) == filepath.Clean(sourcePath) {
		return PlanAction{}
	}
	reserved[destPath] = struct{}{}
	return PlanAction{
		PlanID:          planID,
		SourcePath:      sourcePath,
		DestinationPath: destPath,
		Classification:  filepath.ToSlash(decision.Classification),
		DecisionSource:  decision.DecisionSource,
		Confidence:      decision.Confidence,
		ConfidenceBand:  confidenceBand(decision.Confidence),
		RequiresReview:  decision.RequiresReview,
		Evidence:        uniqueNonEmpty(decision.Evidence),
		SourceSizeBytes: info.Size(),
		SourceModTime:   info.ModTime().UTC().Format(time.RFC3339Nano),
	}
}

func (s *Service) SummarizePlan(plan Plan) string {
	engine, ok := s.engines[plan.Profile]
	summaryLines := []string{}
	if ok {
		summaryLines = engine.rules.SummaryLines(plan)
	}
	if len(summaryLines) == 0 {
		summaryLines = []string{fmt.Sprintf("* %d planned file moves", len(plan.Actions))}
	}
	high, medium, low := confidenceBreakdown(plan.Actions)
	previewGroups := buildPreviewGroups(plan.Actions, s.previewLimit)

	lines := []string{
		"Audio organization plan",
		"",
		"Root:",
		plan.Root,
		"",
		"Scope:",
		"Only audio files inside root",
		"",
		"Summary:",
		"",
	}
	lines = append(lines, summaryLines...)
	lines = append(lines, "", "Preview (sample of planned moves):", "")
	for _, group := range previewGroups {
		lines = append(lines, "-> "+group.destination+"/", "")
		for _, item := range group.items {
			lines = append(lines, "* "+item.filename)
			lines = append(lines, "  reason: "+item.reason)
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		"Confidence breakdown:",
		fmt.Sprintf("High confidence: %d files", high),
		fmt.Sprintf("Medium confidence: %d files", medium),
		fmt.Sprintf("Low confidence: %d files", low),
		"",
		"Progress:",
		fmt.Sprintf("- scanned %d files", plan.Stats.ScannedFiles),
		fmt.Sprintf("- rule-classified %d files", plan.Stats.RuleClassifiedFiles),
		fmt.Sprintf("- queued %d ambiguous files", plan.Stats.QueuedAmbiguousFiles),
		fmt.Sprintf("- model candidates %d files", plan.Stats.ModelCandidateFiles),
		fmt.Sprintf("- model batches completed %d/%d", plan.Stats.ModelBatchesCompleted, plan.Stats.ModelBatchesTotal),
	)
	if plan.Stats.ModelBatchTimeouts > 0 {
		lines = append(lines, fmt.Sprintf("- model timeouts: %d", plan.Stats.ModelBatchTimeouts))
	}
	if plan.Stats.ReviewByBudgetFiles > 0 {
		lines = append(lines, "", fmt.Sprintf("%d ambiguous files were not model-classified due to time budget and were routed to review.", plan.Stats.ReviewByBudgetFiles))
	}
	lines = append(lines,
		"",
		fmt.Sprintf("Nothing outside %s will be touched.", plan.Root),
		fmt.Sprintf("Plan file: %s", plan.PlanPath),
		fmt.Sprintf("Report file: %s", plan.ReportPath),
	)
	return strings.Join(lines, "\n")
}

type previewGroup struct {
	destination string
	items       []previewItem
}

type previewItem struct {
	filename string
	reason   string
}

func buildPreviewGroups(actions []PlanAction, perCategory int) []previewGroup {
	grouped := make(map[string][]PlanAction, 16)
	for _, action := range actions {
		grouped[action.Classification] = append(grouped[action.Classification], action)
	}
	destinations := make([]string, 0, len(grouped))
	for destination := range grouped {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	groups := make([]previewGroup, 0, len(destinations))
	for _, destination := range destinations {
		items := grouped[destination]
		sort.Slice(items, func(i, j int) bool {
			baseI := strings.ToLower(filepath.Base(items[i].SourcePath))
			baseJ := strings.ToLower(filepath.Base(items[j].SourcePath))
			if baseI == baseJ {
				return items[i].SourcePath < items[j].SourcePath
			}
			return baseI < baseJ
		})
		limit := perCategory
		if limit <= 0 {
			limit = 5
		}
		if len(items) < limit {
			limit = len(items)
		}
		previewItems := make([]previewItem, 0, limit)
		for idx := 0; idx < limit; idx++ {
			action := items[idx]
			previewItems = append(previewItems, previewItem{
				filename: filepath.Base(action.SourcePath),
				reason:   summarizeEvidence(action.Evidence),
			})
		}
		groups = append(groups, previewGroup{destination: destination, items: previewItems})
	}
	return groups
}

func summarizeEvidence(evidence []string) string {
	if len(evidence) == 0 {
		return "no evidence"
	}
	if len(evidence) == 1 {
		return evidence[0]
	}
	return evidence[0] + "; " + evidence[1]
}

func confidenceBreakdown(actions []PlanAction) (high, medium, low int) {
	for _, action := range actions {
		switch confidenceBand(action.Confidence) {
		case "high":
			high++
		case "medium":
			medium++
		default:
			low++
		}
	}
	return high, medium, low
}

func (s *Service) ExecutePlan(req ExecutePlanRequest) (ExecutePlanResult, error) {
	if !req.Approved {
		return ExecutePlanResult{}, errors.New("execution requires explicit approval")
	}
	plan := req.Plan
	if strings.TrimSpace(plan.Root) == "" {
		return ExecutePlanResult{}, errors.New("plan root is required")
	}
	if len(plan.Actions) == 0 {
		return ExecutePlanResult{PlanID: plan.PlanID}, nil
	}
	if err := s.appendPlanEvent(plan.PlanPath, planEvent{
		Timestamp: s.now().UTC().Format(time.RFC3339Nano),
		Event:     "organize_plan_approved",
		PlanID:    plan.PlanID,
		RequestID: plan.RequestID,
		Status:    "approved",
	}); err != nil {
		return ExecutePlanResult{}, err
	}

	ts := s.now().UTC()
	reportDir, err := cleanup.ReportDirForRoot(plan.Root)
	if err != nil {
		return ExecutePlanResult{}, err
	}
	execPath := filepath.Join(reportDir, fmt.Sprintf("%s_execution_%s.md", plan.Profile, ts.Format("20060102-150405")))
	moved := make([]string, 0, len(plan.Actions))
	executed, skipped := 0, 0

	for _, action := range plan.Actions {
		status := "executed"
		destPath := action.DestinationPath
		if !pathWithinRoot(action.SourcePath, plan.Root) || !pathWithinRoot(destPath, plan.Root) {
			status = "skipped_outside_scope"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		sourceInfo, err := os.Stat(action.SourcePath)
		if err != nil {
			status = "skipped_source_missing"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		if sourceInfo.Size() != action.SourceSizeBytes || !sameModTime(sourceInfo.ModTime(), action.SourceModTime) {
			status = "skipped_source_changed"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		if _, err := os.Stat(destPath); err == nil {
			status = "skipped_destination_exists"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			status = "skipped_mkdir_failed"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		if err := os.Rename(action.SourcePath, destPath); err != nil {
			status = "skipped_move_failed"
			skipped++
			s.logMoveEvent(plan, action, status)
			continue
		}
		executed++
		moved = append(moved, fmt.Sprintf("%s -> %s", action.SourcePath, destPath))
		s.logMoveEvent(plan, action, status)
	}

	_ = s.appendPlanEvent(plan.PlanPath, planEvent{
		Timestamp: s.now().UTC().Format(time.RFC3339Nano),
		Event:     "organize_execution_completed",
		PlanID:    plan.PlanID,
		RequestID: plan.RequestID,
		Status:    fmt.Sprintf("executed=%d skipped=%d", executed, skipped),
	})
	if err := s.writeExecutionReport(execPath, plan, executed, skipped, moved); err != nil {
		return ExecutePlanResult{}, err
	}

	emptyCount, emptyReportPath, err := s.writeEmptyFolderReport(plan)
	if err != nil {
		return ExecutePlanResult{}, err
	}
	return ExecutePlanResult{
		PlanID:                plan.PlanID,
		Executed:              executed,
		Skipped:               skipped,
		MovedPaths:            moved,
		ExecutionLogPath:      execPath,
		EmptyFolderCount:      emptyCount,
		EmptyFolderReportPath: emptyReportPath,
	}, nil
}

func (s *Service) writeEmptyFolderReport(plan Plan) (int, string, error) {
	cleanupService := cleanup.NewService(s.audit)
	scopes := []cleanup.Scope{
		{Label: "Inside root", Root: plan.Root},
	}
	if strings.TrimSpace(plan.ContextRoot) != "" {
		scopes = append(scopes, cleanup.Scope{
			Label: "Inside context-root",
			Root:  plan.ContextRoot,
		})
	}
	report, err := cleanupService.GenerateEmptyFoldersReport(cleanup.ReportRequest{
		Scopes: scopes,
	})
	if err != nil {
		return 0, "", err
	}
	return report.Total, report.ReportPath, nil
}

func (s *Service) logMoveEvent(plan Plan, action PlanAction, status string) {
	_ = s.appendPlanEvent(plan.PlanPath, planEvent{
		Timestamp:      s.now().UTC().Format(time.RFC3339Nano),
		Event:          "organize_file_move_executed",
		PlanID:         plan.PlanID,
		RequestID:      plan.RequestID,
		Source:         action.SourcePath,
		Destination:    action.DestinationPath,
		Classification: action.Classification,
		DecisionSource: action.DecisionSource,
		Confidence:     action.Confidence,
		Status:         status,
		Evidence:       action.Evidence,
	})
}

func (s *Service) SavePendingPlan(plan Plan) error {
	path, err := pendingPlanPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload := PendingPlan{SavedAt: s.now().UTC().Format(time.RFC3339Nano), Plan: plan}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (s *Service) LoadPendingPlan() (PendingPlan, error) {
	path, err := pendingPlanPath()
	if err != nil {
		return PendingPlan{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return PendingPlan{}, err
	}
	var pending PendingPlan
	if err := json.Unmarshal(raw, &pending); err != nil {
		return PendingPlan{}, err
	}
	return pending, nil
}

func (s *Service) ClearPendingPlan() error {
	path, err := pendingPlanPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func pendingPlanPath() (string, error) {
	cfgPath, err := settings.ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "organize_audio_pending.json"), nil
}

func (s *Service) writePlanJSONL(plan Plan) error {
	if err := os.MkdirAll(filepath.Dir(plan.PlanPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(plan.PlanPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	writer := bufio.NewWriter(f)
	if err := writeJSONLine(writer, planEvent{
		Timestamp: s.now().UTC().Format(time.RFC3339Nano),
		Event:     "organize_plan_created",
		PlanID:    plan.PlanID,
		RequestID: plan.RequestID,
		Status: fmt.Sprintf(
			"actions=%d scanned=%d queued_ambiguous=%d model_candidates=%d fallback_review=%d",
			len(plan.Actions),
			plan.Stats.ScannedFiles,
			plan.Stats.QueuedAmbiguousFiles,
			plan.Stats.ModelCandidateFiles,
			plan.Stats.FallbackAmbiguousNotModelClassified,
		),
	}); err != nil {
		return err
	}
	for _, action := range plan.Actions {
		entry := struct {
			Timestamp      string   `json:"timestamp"`
			Event          string   `json:"event"`
			PlanID         string   `json:"plan_id"`
			RequestID      string   `json:"request_id"`
			SourcePath     string   `json:"source_path"`
			Destination    string   `json:"destination_path"`
			Classification string   `json:"classification"`
			DecisionSource string   `json:"decision_source"`
			Confidence     float64  `json:"confidence"`
			ConfidenceBand string   `json:"confidence_band"`
			RequiresReview bool     `json:"requires_review"`
			Evidence       []string `json:"evidence"`
			Status         string   `json:"status"`
		}{
			Timestamp:      s.now().UTC().Format(time.RFC3339Nano),
			Event:          "organize_file_classified",
			PlanID:         action.PlanID,
			RequestID:      plan.RequestID,
			SourcePath:     action.SourcePath,
			Destination:    action.DestinationPath,
			Classification: action.Classification,
			DecisionSource: action.DecisionSource,
			Confidence:     action.Confidence,
			ConfidenceBand: action.ConfidenceBand,
			RequiresReview: action.RequiresReview,
			Evidence:       action.Evidence,
			Status:         "planned",
		}
		if err := writeJSONLine(writer, entry); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.LogToolCall("organize_audio_generate_plan", nil, plan.PlanPath, nil)
	}
	return nil
}

func (s *Service) writePlanReport(plan Plan) error {
	var builder strings.Builder
	builder.WriteString("# Audio Organization Report\n\n")
	builder.WriteString(fmt.Sprintf("Plan ID: %s\n", plan.PlanID))
	builder.WriteString(fmt.Sprintf("Root: %s\n", plan.Root))
	builder.WriteString(fmt.Sprintf("Context root: %s\n", plan.ContextRoot))
	builder.WriteString(fmt.Sprintf("Created: %s\n\n", plan.CreatedAt))
	builder.WriteString("## Summary\n\n")
	builder.WriteString(s.SummarizePlan(plan))
	builder.WriteString("\n")
	if err := os.WriteFile(plan.ReportPath, []byte(builder.String()), 0o644); err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.LogToolCall("organize_audio_write_report", nil, plan.ReportPath, nil)
	}
	return nil
}

func (s *Service) writeExecutionReport(path string, plan Plan, executed, skipped int, moved []string) error {
	var builder strings.Builder
	builder.WriteString("# Audio Organization Execution\n\n")
	builder.WriteString(fmt.Sprintf("Plan ID: %s\n", plan.PlanID))
	builder.WriteString(fmt.Sprintf("Root: %s\n", plan.Root))
	builder.WriteString(fmt.Sprintf("Executed moves: %d\n", executed))
	builder.WriteString(fmt.Sprintf("Skipped: %d\n", skipped))
	builder.WriteString(fmt.Sprintf("Completed: %s\n\n", s.now().UTC().Format(time.RFC3339Nano)))
	builder.WriteString("## Moves\n")
	if len(moved) == 0 {
		builder.WriteString("- No files moved.\n")
	} else {
		for _, item := range moved {
			builder.WriteString("- " + item + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o644); err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.LogToolCall("organize_audio_execute_plan", nil, path, nil)
	}
	return nil
}

func (s *Service) appendPlanEvent(path string, entry planEvent) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return err
}

func (s *Service) InspectPlan(planPath, filter string) (string, error) {
	actions, err := LoadPlanActionsFromJSONL(planPath)
	if err != nil {
		return "", err
	}
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter != "" {
		filtered := make([]PlanAction, 0, len(actions))
		for _, action := range actions {
			target := strings.ToLower(strings.Join([]string{
				action.Classification, action.SourcePath, filepath.Base(action.SourcePath), strings.Join(action.Evidence, " "),
			}, " "))
			if strings.Contains(target, filter) {
				filtered = append(filtered, action)
			}
		}
		actions = filtered
	}
	grouped := make(map[string][]PlanAction, 16)
	for _, action := range actions {
		grouped[action.Classification] = append(grouped[action.Classification], action)
	}
	destinations := make([]string, 0, len(grouped))
	for destination := range grouped {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Plan inspection: %s\n", planPath))
	builder.WriteString(fmt.Sprintf("Files: %d\n\n", len(actions)))
	for _, destination := range destinations {
		builder.WriteString(destination + "/\n")
		items := grouped[destination]
		sort.Slice(items, func(i, j int) bool { return items[i].SourcePath < items[j].SourcePath })
		for _, item := range items {
			builder.WriteString(fmt.Sprintf("- %s (confidence: %.2f, source: %s)\n", filepath.Base(item.SourcePath), item.Confidence, item.DecisionSource))
			builder.WriteString(fmt.Sprintf("  reason: %s\n", strings.Join(item.Evidence, "; ")))
		}
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String()), nil
}

func LoadPlanActionsFromJSONL(planPath string) ([]PlanAction, error) {
	f, err := os.Open(planPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	actions := make([]PlanAction, 0, 128)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		if strings.TrimSpace(toString(raw["event"])) != "organize_file_classified" {
			continue
		}
		conf := toFloat64(raw["confidence"])
		actions = append(actions, PlanAction{
			PlanID:          toString(raw["plan_id"]),
			SourcePath:      toString(raw["source_path"]),
			DestinationPath: toString(raw["destination_path"]),
			Classification:  toString(raw["classification"]),
			DecisionSource:  firstNonEmpty(toString(raw["decision_source"]), "rule"),
			Confidence:      conf,
			ConfidenceBand:  firstNonEmpty(toString(raw["confidence_band"]), confidenceBand(conf)),
			RequiresReview:  toBool(raw["requires_review"]),
			Evidence:        toStringSlice(raw["evidence"]),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].Classification == actions[j].Classification {
			return actions[i].SourcePath < actions[j].SourcePath
		}
		return actions[i].Classification < actions[j].Classification
	})
	return actions, nil
}

func cacheKeyFor(path string, info os.FileInfo) string {
	return fmt.Sprintf("%s|%d|%s", filepath.Clean(path), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}

func loadModelCache() (map[string]cachedDecision, string, error) {
	cfgPath, err := settings.ConfigPath()
	if err != nil {
		return map[string]cachedDecision{}, "", err
	}
	path := filepath.Join(filepath.Dir(cfgPath), "organize_audio_model_cache.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]cachedDecision{}, path, nil
		}
		return map[string]cachedDecision{}, path, err
	}
	var payload modelCacheFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return map[string]cachedDecision{}, path, nil
	}
	if payload.Entries == nil {
		payload.Entries = map[string]cachedDecision{}
	}
	return payload.Entries, path, nil
}

func saveModelCache(path string, entries map[string]cachedDecision) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload := modelCacheFile{Entries: entries}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func writeJSONLine(w *bufio.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

func normalizeDecision(decision ClassificationDecision) ClassificationDecision {
	decision.Classification = filepath.ToSlash(strings.TrimSpace(decision.Classification))
	if decision.Classification == "" {
		decision.Classification = "review/ambiguous"
	}
	if decision.Confidence < 0 {
		decision.Confidence = 0
	}
	if decision.Confidence > 1 {
		decision.Confidence = 1
	}
	if strings.TrimSpace(decision.DecisionSource) == "" {
		decision.DecisionSource = "rule"
	}
	decision.Evidence = uniqueNonEmpty(decision.Evidence)
	if len(decision.Evidence) == 0 {
		decision.Evidence = []string{"no explicit evidence provided"}
	}
	return decision
}

func uniqueDestination(target string, reserved map[string]struct{}) string {
	base := target
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(filepath.Base(base), ext)
	dir := filepath.Dir(base)
	candidate := base
	index := 1
	for {
		if _, ok := reserved[candidate]; ok {
			candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, index, ext))
			index++
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, index, ext))
			index++
			continue
		}
		return candidate
	}
}

func sameModTime(now time.Time, recorded string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, recorded)
	if err != nil {
		return false
	}
	return now.UTC().Equal(parsed.UTC())
}

func uniqueNonEmpty(values []string) []string {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func toString(v any) string {
	value, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func toFloat64(v any) float64 {
	switch typed := v.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return 0
	}
}

func toBool(v any) bool {
	value, ok := v.(bool)
	return ok && value
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]string); ok {
			return typed
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value := toString(item)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
