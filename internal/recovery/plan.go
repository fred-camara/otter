package recovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

type LogEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type Entry struct {
	CurrentPath         string     `json:"current_path"`
	ProposedDestination string     `json:"proposed_destination"`
	Confidence          Confidence `json:"confidence"`
	Reason              string     `json:"reason"`
}

type Plan struct {
	RootPath      string  `json:"root_path"`
	DryRun        bool    `json:"dry_run"`
	ExecutionNote string  `json:"execution_note"`
	LogSignals    int     `json:"log_signals"`
	Entries       []Entry `json:"entries"`
	NeedsReview   int     `json:"needs_review"`
	Recoverable   int     `json:"recoverable"`
	UnchangedHint int     `json:"unchanged_hint"`
}

func ParseLogEntries(text string) []LogEntry {
	segments := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == ';'
	})
	entries := make([]LogEntry, 0, len(segments))
	for _, segment := range segments {
		line := strings.TrimSpace(segment)
		if line == "" || !strings.Contains(line, "->") {
			continue
		}
		parts := strings.SplitN(line, "->", 2)
		if len(parts) != 2 {
			continue
		}
		source := strings.Trim(strings.TrimSpace(parts[0]), "\"'`")
		target := strings.Trim(strings.TrimSpace(parts[1]), "\"'`")
		if source == "" || target == "" {
			continue
		}
		entries = append(entries, LogEntry{Source: filepath.Clean(source), Target: filepath.Clean(target)})
	}
	return entries
}

func Generate(rootPath string, logs []LogEntry) (Plan, error) {
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve root: %w", err)
	}
	root = filepath.Clean(root)

	files, err := collectFiles(root)
	if err != nil {
		return Plan{}, err
	}

	exactByTarget := make(map[string]string, len(logs))
	basenameSources := make(map[string]map[string]struct{}, len(logs))
	for _, item := range logs {
		source := filepath.Clean(item.Source)
		target := filepath.Clean(item.Target)
		exactByTarget[target] = source
		base := strings.ToLower(filepath.Base(target))
		if base == "" {
			continue
		}
		if basenameSources[base] == nil {
			basenameSources[base] = map[string]struct{}{}
		}
		basenameSources[base][source] = struct{}{}
	}

	entries := make([]Entry, 0, len(files))
	needsReview := 0
	recoverable := 0
	unchanged := 0

	for _, current := range files {
		if source, ok := exactByTarget[current]; ok {
			entries = append(entries, Entry{
				CurrentPath:         current,
				ProposedDestination: source,
				Confidence:          ConfidenceHigh,
				Reason:              "Matched exact move-log target",
			})
			recoverable++
			continue
		}

		base := strings.ToLower(filepath.Base(current))
		if candidates, ok := basenameSources[base]; ok && len(candidates) == 1 {
			for source := range candidates {
				entries = append(entries, Entry{
					CurrentPath:         current,
					ProposedDestination: source,
					Confidence:          ConfidenceHigh,
					Reason:              "Matched unique filename in move logs",
				})
			}
			recoverable++
			continue
		}

		destination, confidence, reason := heuristicDestination(root, current)
		if confidence == ConfidenceLow {
			needsReview++
		} else {
			recoverable++
		}
		if destination == current {
			unchanged++
		}

		entries = append(entries, Entry{
			CurrentPath:         current,
			ProposedDestination: destination,
			Confidence:          confidence,
			Reason:              reason,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Confidence == entries[j].Confidence {
			if entries[i].CurrentPath == entries[j].CurrentPath {
				return entries[i].ProposedDestination < entries[j].ProposedDestination
			}
			return entries[i].CurrentPath < entries[j].CurrentPath
		}
		return confidenceRank(entries[i].Confidence) < confidenceRank(entries[j].Confidence)
	})

	return Plan{
		RootPath:      root,
		DryRun:        true,
		ExecutionNote: "If execution is later confirmed, generate a timestamped pre-move manifest before applying moves.",
		LogSignals:    len(logs),
		Entries:       entries,
		NeedsReview:   needsReview,
		Recoverable:   recoverable,
		UnchangedHint: unchanged,
	}, nil
}

func PlanJSON(plan Plan) (string, error) {
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

func PlanMarkdown(plan Plan) string {
	b := strings.Builder{}
	b.WriteString("# Recovery Plan (Dry Run)\n\n")
	b.WriteString("No file moves were executed.\n\n")
	b.WriteString(fmt.Sprintf("Root: `%s`\n\n", plan.RootPath))
	b.WriteString(fmt.Sprintf("- Recoverable entries: %d\n", plan.Recoverable))
	b.WriteString(fmt.Sprintf("- Needs review: %d\n", plan.NeedsReview))
	b.WriteString(fmt.Sprintf("- Log signals used: %d\n\n", plan.LogSignals))
	b.WriteString("Future execution note: ")
	b.WriteString(plan.ExecutionNote)
	b.WriteString("\n\n")
	b.WriteString("## Proposed Moves\n\n")
	b.WriteString("| Current Path | Proposed Destination | Confidence | Reason |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, entry := range plan.Entries {
		b.WriteString(fmt.Sprintf("| `%s` | `%s` | %s | %s |\n",
			entry.CurrentPath,
			entry.ProposedDestination,
			entry.Confidence,
			entry.Reason,
		))
	}
	return b.String()
}

func collectFiles(root string) ([]string, error) {
	files := make([]string, 0, 64)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		clean := filepath.Clean(path)
		if strings.EqualFold(filepath.Base(clean), "recovery_plan.md") || strings.EqualFold(filepath.Base(clean), "recovery_plan.json") {
			return nil
		}
		files = append(files, clean)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan root files: %w", err)
	}
	sort.Strings(files)
	return files, nil
}

func heuristicDestination(root, current string) (string, Confidence, string) {
	rel, err := filepath.Rel(root, current)
	if err != nil {
		return filepath.Join(root, "Needs Review", filepath.Base(current)), ConfidenceLow, "Could not compute stable relative path"
	}
	rel = filepath.Clean(rel)
	ext := strings.ToLower(filepath.Ext(current))
	format := "Other"
	switch ext {
	case ".mp3":
		format = "MP3"
	case ".wav", ".aiff", ".aif", ".flac":
		format = "Lossless"
	}

	pack, signal := inferPackSignal(rel)
	if signal == "" {
		return filepath.Join(root, "Needs Review", rel), ConfidenceLow, "Insufficient signal to reconstruct original pack"
	}

	destination := filepath.Join(root, "Recovered", pack, format, rel)
	return destination, ConfidenceMedium, signal
}

func inferPackSignal(rel string) (string, string) {
	lower := strings.ToLower(rel)
	if strings.Contains(lower, "cymatics") {
		pack := firstMatchingPart(rel, func(part string) bool {
			return strings.Contains(strings.ToLower(part), "cymatics")
		})
		if pack == "" {
			pack = "Cymatics"
		}
		return safeGroup(pack), "Detected Cymatics naming pattern"
	}

	parent := filepath.Base(filepath.Dir(rel))
	if validGroup(parent) {
		return safeGroup(parent), "Reused parent folder as source-pack hint"
	}

	base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	prefix := splitPrefix(base)
	if validGroup(prefix) {
		return safeGroup(prefix), "Grouped by filename prefix"
	}

	return "", ""
}

func splitPrefix(base string) string {
	for _, sep := range []string{" - ", "_", "-", " "} {
		if idx := strings.Index(base, sep); idx > 0 {
			return strings.TrimSpace(base[:idx])
		}
	}
	return ""
}

func validGroup(group string) bool {
	g := strings.ToLower(strings.TrimSpace(group))
	if g == "" || g == "." || g == ".." {
		return false
	}
	for _, generic := range []string{"audio", "samples", "sample", "downloads", "desktop", "documents", "other", "recovered", "needs review", "needs_review"} {
		if g == generic {
			return false
		}
	}
	return len(g) >= 3
}

func safeGroup(group string) string {
	clean := strings.TrimSpace(group)
	clean = strings.ReplaceAll(clean, string(os.PathSeparator), " ")
	clean = strings.ReplaceAll(clean, "\\", " ")
	clean = strings.Join(strings.Fields(clean), " ")
	if clean == "" {
		return "Unknown"
	}
	return clean
}

func firstMatchingPart(rel string, match func(string) bool) string {
	parts := strings.Split(filepath.Clean(rel), string(os.PathSeparator))
	for _, part := range parts {
		if match(part) {
			return part
		}
	}
	return ""
}

func confidenceRank(value Confidence) int {
	switch value {
	case ConfidenceHigh:
		return 0
	case ConfidenceMedium:
		return 1
	default:
		return 2
	}
}
