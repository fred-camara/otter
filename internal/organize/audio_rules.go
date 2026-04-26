package organize

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"otter/internal/model"
)

var (
	audioExtSet = map[string]struct{}{
		".mp3": {}, ".wav": {}, ".aif": {}, ".aiff": {}, ".m4a": {}, ".aac": {}, ".flac": {}, ".ogg": {},
	}
	likelyMusicExtSet = map[string]struct{}{
		".mp3": {}, ".m4a": {}, ".aac": {}, ".flac": {}, ".ogg": {},
	}
	likelySampleExtSet = map[string]struct{}{
		".wav": {}, ".aif": {}, ".aiff": {},
	}
	audioAllowedClassifications = map[string]struct{}{
		"music/known_artist_title":   {},
		"music/internet_downloads":   {},
		"music/unknown_music":        {},
		"samples/drums/808s":         {},
		"samples/drums/kicks":        {},
		"samples/drums/snares":       {},
		"samples/drums/hihats":       {},
		"samples/drums/percussion":   {},
		"samples/loops":              {},
		"samples/instruments/bass":   {},
		"samples/instruments/keys":   {},
		"samples/instruments/synths": {},
		"samples/instruments/guitar": {},
		"samples/vocals":             {},
		"samples/fx":                 {},
		"samples/unknown_samples":    {},
		"review/ambiguous":           {},
		"review/low_confidence":      {},
	}
)

type AudioRules struct{}

func NewAudioRules() *AudioRules { return &AudioRules{} }

func (r *AudioRules) Profile() string { return ProfileAudio }
func (r *AudioRules) DefaultRoot() string {
	return "~/Downloads/audio"
}
func (r *AudioRules) DefaultContextRoot() string {
	return "~/Downloads"
}

func (r *AudioRules) PrepareContext(_, contextRoot string) (map[string]any, error) {
	folders := collectContextFolderNames(contextRoot)
	return map[string]any{"context_folders": folders}, nil
}

func (r *AudioRules) IsCandidateFile(path string, entry os.DirEntry) bool {
	if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
		return false
	}
	_, ok := audioExtSet[strings.ToLower(filepath.Ext(path))]
	return ok
}

func (r *AudioRules) BuildDestination(root, sourcePath string, decision ClassificationDecision) string {
	return filepath.Join(root, filepath.FromSlash(decision.Classification), filepath.Base(sourcePath))
}

func (r *AudioRules) SummaryLines(plan Plan) []string {
	mp3 := 0
	wav := 0
	review := 0
	for _, action := range plan.Actions {
		ext := strings.ToLower(filepath.Ext(action.SourcePath))
		if ext == ".mp3" {
			mp3++
		}
		if ext == ".wav" {
			wav++
		}
		if strings.HasPrefix(action.Classification, "review/") || action.RequiresReview {
			review++
		}
	}
	return []string{
		fmt.Sprintf("* %d MP3 -> music/", mp3),
		fmt.Sprintf("* %d WAV -> samples/", wav),
		fmt.Sprintf("* %d -> review/", review),
	}
}

type AudioHybridClassifier struct {
	rule  *AudioRuleClassifier
	model model.Interface
}

func NewAudioHybridClassifier(modelGen model.Interface) *AudioHybridClassifier {
	return &AudioHybridClassifier{
		rule:  NewAudioRuleClassifier(),
		model: modelGen,
	}
}

func (c *AudioHybridClassifier) Classify(input ClassificationInput) ClassificationDecision {
	return c.rule.Classify(input)
}

func (c *AudioHybridClassifier) InferOrganizationSpec(sample StrategySample, timeout time.Duration) (OrganizationSpec, error) {
	if c.model == nil {
		return OrganizationSpec{Source: "rule-default", Notes: []string{"model unavailable"}}, nil
	}
	prompt := buildStrategyPrompt(sample)
	raw, err := callModelWithTimeout(c.model, prompt, timeout)
	if err != nil {
		return OrganizationSpec{}, err
	}
	var payload struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return OrganizationSpec{}, err
	}
	notes := uniqueNonEmpty(payload.Notes)
	if len(notes) == 0 {
		notes = []string{"use conservative audio categorization and route uncertain files to review"}
	}
	return OrganizationSpec{Source: "model", Notes: notes}, nil
}

func (c *AudioHybridClassifier) ClassifyAmbiguousBatch(inputs []ClassificationInput, spec OrganizationSpec, timeout time.Duration) ([]ClassificationDecision, error) {
	if c.model == nil {
		return nil, errors.New("model unavailable")
	}
	prompt := buildBatchClassificationPrompt(inputs, spec)
	raw, err := callModelWithTimeout(c.model, prompt, timeout)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Classifications []struct {
			ID             int      `json:"id"`
			Classification string   `json:"classification"`
			Confidence     float64  `json:"confidence"`
			Evidence       []string `json:"evidence"`
		} `json:"classifications"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return nil, err
	}

	byID := make(map[int]ClassificationDecision, len(payload.Classifications))
	for _, item := range payload.Classifications {
		classification := strings.TrimSpace(item.Classification)
		if _, ok := audioAllowedClassifications[classification]; !ok {
			continue
		}
		confidence := item.Confidence
		if confidence <= 0 || confidence > 1 {
			continue
		}
		byID[item.ID] = ClassificationDecision{
			Classification: classification,
			Confidence:     confidence,
			DecisionSource: "hybrid",
			RequiresReview: strings.HasPrefix(classification, "review/"),
			Evidence:       uniqueNonEmpty(item.Evidence),
		}
	}

	out := make([]ClassificationDecision, 0, len(inputs))
	for idx, input := range inputs {
		decision, ok := byID[idx]
		if !ok {
			out = append(out, ClassificationDecision{
				Classification: "review/ambiguous",
				Confidence:     0.40,
				DecisionSource: "hybrid",
				RequiresReview: true,
				Evidence: []string{
					"model did not return trusted classification",
					"fallback to review",
					"filename: " + filepath.Base(input.SourcePath),
				},
			})
			continue
		}
		if len(decision.Evidence) == 0 {
			decision.Evidence = []string{"model-assisted ambiguous classification"}
		}
		out = append(out, decision)
	}
	return out, nil
}

type AudioRuleClassifier struct{}

func NewAudioRuleClassifier() *AudioRuleClassifier { return &AudioRuleClassifier{} }

func (c *AudioRuleClassifier) Classify(input ClassificationInput) ClassificationDecision {
	path := input.SourcePath
	root := input.Root
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(base))
	nameOnly := strings.TrimSuffix(base, ext)
	relativeParent := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(filepath.Dir(path), root)))
	context := strings.Join([]string{nameOnly, relativeParent}, " ")
	contextFolders := stringSliceContext(input.Context, "context_folders")

	if isAmbiguousFilename(nameOnly) {
		return ClassificationDecision{
			Classification: "review/ambiguous",
			Confidence:     0.20,
			DecisionSource: "rule",
			RequiresReview: true,
			Evidence:       []string{"filename lacks semantic tokens", "rule fallback to review"},
		}
	}
	if strings.Contains(nameOnly, "youtube_") || strings.Contains(nameOnly, "onlymp3.to") {
		return ClassificationDecision{
			Classification: "music/internet_downloads",
			Confidence:     0.95,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "filename matches downloader pattern"},
		}
	}
	if drumsClass, ok := matchDrumsClass(context); ok {
		return ClassificationDecision{
			Classification: drumsClass,
			Confidence:     0.93,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "filename contains drum keyword"},
		}
	}
	if strings.Contains(context, "loop") || regexp.MustCompile(`\b\d{2,3}bpm\b`).MatchString(context) {
		return ClassificationDecision{
			Classification: "samples/loops",
			Confidence:     0.90,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "contains loop/bpm token"},
		}
	}
	if strings.Contains(context, "vocal") || strings.Contains(context, "vox") || strings.Contains(context, "adlib") || strings.Contains(context, "ad-libs") {
		return ClassificationDecision{
			Classification: "samples/vocals",
			Confidence:     0.90,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "contains vocal token"},
		}
	}
	if strings.Contains(context, "fx") || strings.Contains(context, "riser") || strings.Contains(context, "sweep") || strings.Contains(context, "impact") {
		return ClassificationDecision{
			Classification: "samples/fx",
			Confidence:     0.88,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "contains fx token"},
		}
	}
	if instClass, ok := matchInstrumentClass(context); ok {
		return ClassificationDecision{
			Classification: instClass,
			Confidence:     0.90,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "contains instrument token"},
		}
	}

	if _, ok := likelyMusicExtSet[ext]; ok {
		if strings.Contains(filepath.Base(path), " - ") {
			return ClassificationDecision{
				Classification: "music/known_artist_title",
				Confidence:     0.91,
				DecisionSource: "rule",
				Evidence:       []string{"extension is " + ext, "artist-title naming pattern"},
			}
		}
		return ClassificationDecision{
			Classification: "music/unknown_music",
			Confidence:     0.72,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "music-skewed extension rule"},
		}
	}
	if _, ok := likelySampleExtSet[ext]; ok {
		pack := inferPackName(path, root, contextFolders)
		if pack != "" {
			return ClassificationDecision{
				Classification: "samples/packs/" + pack,
				Confidence:     0.68,
				DecisionSource: "rule",
				Evidence:       []string{"extension is " + ext, "pack-name inference from folders/context"},
			}
		}
		return ClassificationDecision{
			Classification: "samples/unknown_samples",
			Confidence:     0.64,
			DecisionSource: "rule",
			Evidence:       []string{"extension is " + ext, "sample-skewed extension rule"},
		}
	}
	return ClassificationDecision{
		Classification: "review/ambiguous",
		Confidence:     0.25,
		DecisionSource: "rule",
		RequiresReview: true,
		Evidence:       []string{"unsupported classification signal"},
	}
}

func buildStrategyPrompt(sample StrategySample) string {
	return fmt.Sprintf(`Return strict JSON only:
{"notes":["short strategy note 1","short strategy note 2"]}

Context folders: %s
Top extensions: %s
Examples: %s`, strings.Join(sample.ContextFolders, ", "), strings.Join(sample.TopExtensions, ", "), strings.Join(sample.Examples, " | "))
}

func buildBatchClassificationPrompt(inputs []ClassificationInput, spec OrganizationSpec) string {
	type filePayload struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Ext    string `json:"ext"`
		Parent string `json:"parent"`
	}
	files := make([]filePayload, 0, len(inputs))
	for idx, input := range inputs {
		path := input.SourcePath
		files = append(files, filePayload{
			ID:     idx,
			Name:   filepath.Base(path),
			Ext:    strings.ToLower(filepath.Ext(path)),
			Parent: filepath.Base(filepath.Dir(path)),
		})
	}
	rawFiles, _ := json.Marshal(files)
	notes := strings.Join(spec.Notes, "; ")
	if strings.TrimSpace(notes) == "" {
		notes = "prefer conservative routing to review when uncertain"
	}
	return fmt.Sprintf(`Return strict JSON only.
Classify each file id to one allowed label:
music/known_artist_title,music/internet_downloads,music/unknown_music,samples/drums/808s,samples/drums/kicks,samples/drums/snares,samples/drums/hihats,samples/drums/percussion,samples/loops,samples/instruments/bass,samples/instruments/keys,samples/instruments/synths,samples/instruments/guitar,samples/vocals,samples/fx,samples/unknown_samples,review/ambiguous,review/low_confidence

Strategy notes: %s
Input files JSON: %s

Output schema:
{"classifications":[{"id":0,"classification":"review/ambiguous","confidence":0.0,"evidence":["reason"]}]}`, notes, string(rawFiles))
}

func callModelWithTimeout(m model.Interface, prompt string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := m.Generate(prompt)
		ch <- result{text: text, err: err}
	}()
	select {
	case out := <-ch:
		return out.text, out.err
	case <-time.After(timeout):
		return "", fmt.Errorf("model timeout after %s", timeout)
	}
}

func collectContextFolderNames(contextRoot string) []string {
	entries, err := os.ReadDir(contextRoot)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.TrimSpace(name) == "" || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func inferPackName(path, root string, contextFolders []string) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err == nil {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			name := sanitizePackName(parts[i])
			if name != "" && !isGenericPackToken(name) {
				return name
			}
		}
	}
	base := strings.ToLower(filepath.Base(path))
	for _, folder := range contextFolders {
		token := strings.ToLower(strings.TrimSpace(folder))
		if token == "" || isGenericPackToken(token) {
			continue
		}
		if strings.Contains(base, token) {
			return sanitizePackName(folder)
		}
	}
	return ""
}

func sanitizePackName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	re := regexp.MustCompile(`[^a-z0-9]+`)
	value = re.ReplaceAllString(value, "_")
	return strings.Trim(value, "_")
}

func isGenericPackToken(value string) bool {
	token := strings.ToLower(strings.TrimSpace(value))
	if token == "" {
		return true
	}
	generic := map[string]struct{}{
		"audio": {}, "music": {}, "samples": {}, "sample": {}, "drums": {}, "pack": {}, "packs": {},
		"downloads": {}, "desktop": {}, "documents": {}, "review": {}, "unknown": {}, "loops": {}, "fx": {},
		"instruments": {}, "vocals": {}, "organized": {}, "other": {},
	}
	_, ok := generic[token]
	return ok
}

func matchDrumsClass(text string) (string, bool) {
	switch {
	case strings.Contains(text, "808"):
		return "samples/drums/808s", true
	case strings.Contains(text, "kick"):
		return "samples/drums/kicks", true
	case strings.Contains(text, "snare"):
		return "samples/drums/snares", true
	case strings.Contains(text, "hihat"), strings.Contains(text, " hi hat "), strings.Contains(text, " hh "):
		return "samples/drums/hihats", true
	case strings.Contains(text, "perc"), strings.Contains(text, "shaker"):
		return "samples/drums/percussion", true
	}
	return "", false
}

func matchInstrumentClass(text string) (string, bool) {
	switch {
	case strings.Contains(text, "bass"), strings.Contains(text, "sub"):
		return "samples/instruments/bass", true
	case strings.Contains(text, "keys"), strings.Contains(text, "piano"), strings.Contains(text, "rhodes"):
		return "samples/instruments/keys", true
	case strings.Contains(text, "synth"):
		return "samples/instruments/synths", true
	case strings.Contains(text, "guitar"):
		return "samples/instruments/guitar", true
	}
	return "", false
}

func isAmbiguousFilename(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return true
	}
	numberOnly := regexp.MustCompile(`^[0-9_-]+$`)
	if numberOnly.MatchString(trimmed) {
		return true
	}
	alphaNum := regexp.MustCompile(`[a-zA-Z]`)
	return !alphaNum.MatchString(trimmed)
}

func stringSliceContext(ctx map[string]any, key string) []string {
	raw, ok := ctx[key]
	if !ok || raw == nil {
		return nil
	}
	if typed, ok := raw.([]string); ok {
		return typed
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok && strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}
