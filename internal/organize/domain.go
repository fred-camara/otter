package organize

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ProfileAudio = "audio"

type DomainRules interface {
	Profile() string
	DefaultRoot() string
	DefaultContextRoot() string
	PrepareContext(root, contextRoot string) (map[string]any, error)
	IsCandidateFile(path string, entry os.DirEntry) bool
	BuildDestination(root, sourcePath string, decision ClassificationDecision) string
	SummaryLines(plan Plan) []string
}

type ClassificationStrategy interface {
	Classify(input ClassificationInput) ClassificationDecision
}

type StrategyInferer interface {
	InferOrganizationSpec(sample StrategySample, timeout time.Duration) (OrganizationSpec, error)
}

type AmbiguousBatchClassifier interface {
	ClassifyAmbiguousBatch(inputs []ClassificationInput, spec OrganizationSpec, timeout time.Duration) ([]ClassificationDecision, error)
}

type ClassificationInput struct {
	SourcePath string
	Root       string
	Context    map[string]any
}

type ClassificationDecision struct {
	Classification string
	Confidence     float64
	DecisionSource string
	RequiresReview bool
	Evidence       []string
}

type StrategySample struct {
	ContextFolders []string
	Examples       []string
	TopExtensions  []string
}

type OrganizationSpec struct {
	Source string
	Notes  []string
}

func confidenceBand(confidence float64) string {
	switch {
	case confidence >= 0.85:
		return "high"
	case confidence >= 0.60:
		return "medium"
	default:
		return "low"
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
