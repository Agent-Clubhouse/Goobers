// Package learning defines durable finding-level repass episode contracts.
package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// EpisodeSchema is the versioned wire schema for injected learning episodes.
const EpisodeSchema = "goobers.dev/learning/episode/v1"

const (
	// ActionInstructionOrSkill routes instruction and skill learning to Tutor.
	ActionInstructionOrSkill = "instruction-or-skill"
	// ActionWorkflowOrGate routes workflow and gate learning to Tutor.
	ActionWorkflowOrGate = "workflow-or-gate"
	// ActionTargetedTest routes validation learning to Tutor.
	ActionTargetedTest = "targeted-test-mapping"
	// ActionCodeIssue routes code defects to unapproved issue nomination.
	ActionCodeIssue = "code-issue"
)

const (
	// OutcomeUnresolved means no later result established a disposition.
	OutcomeUnresolved = "unresolved"
	// OutcomeFixed means a later result resolved or suppressed the finding.
	OutcomeFixed = "fixed"
	// OutcomeRepeated means the same finding persisted in a later result.
	OutcomeRepeated = "repeated"
	// OutcomeChangedFailure means a later result failed with different findings.
	OutcomeChangedFailure = "changed-failure"
	// OutcomeEscalated means the run terminated without resolving the finding.
	OutcomeEscalated = "escalated"
	// OutcomeFalseFinding means deterministic evidence disproved the finding.
	OutcomeFalseFinding = "false-finding"
)

// Episode is the bounded, typed artifact injected into a repass. Journal
// pointers remain authoritative; the artifact carries enough identity and
// policy context for the next implementer and reviewer to preserve findings.
type Episode struct {
	Schema             string                       `json:"schema"`
	ID                 string                       `json:"id"`
	SourceRunID        string                       `json:"sourceRunId"`
	SourceSeq          uint64                       `json:"sourceSeq"`
	Workflow           string                       `json:"workflow"`
	Stage              string                       `json:"stage,omitempty"`
	Gate               string                       `json:"gate,omitempty"`
	SourceAttempt      int                          `json:"sourceAttempt"`
	NextAttempt        int                          `json:"nextAttempt"`
	WorkflowDigest     string                       `json:"workflowDigest,omitempty"`
	GooberDigest       string                       `json:"gooberDigest,omitempty"`
	EffectiveVersion   string                       `json:"effectiveVersion,omitempty"`
	Signature          string                       `json:"signature"`
	Classification     apiv1.LearningClassification `json:"classification"`
	RecommendedAction  string                       `json:"recommendedAction"`
	CorrectionFeedback string                       `json:"correctionFeedback,omitempty"`
	Findings           []apiv1.Finding              `json:"findings"`
	Actions            []FindingAction              `json:"actions"`
	Evidence           []apiv1.ArtifactPointer      `json:"evidence,omitempty"`
	Outcome            string                       `json:"outcome"`
}

// FindingAction is the finding-level repass and durable-action contract. The
// episode-level classification/action fields remain a compatibility summary;
// this slice is authoritative when an episode contains heterogeneous findings.
type FindingAction struct {
	FindingID         string                       `json:"findingId"`
	Signature         string                       `json:"signature"`
	Classification    apiv1.LearningClassification `json:"classification"`
	RecommendedAction string                       `json:"recommendedAction"`
	EvidenceDigest    string                       `json:"evidenceDigest,omitempty"`
}

var trailingLocation = regexp.MustCompile(`(?::\d+){1,2}$`)
var digitRun = regexp.MustCompile(`\d+`)

// NormalizeFinding fills the stable learning fields that a reviewer omitted.
func NormalizeFinding(f *apiv1.Finding, gate, evidenceDigest string) {
	if f.LearningClassification == "" || !f.LearningClassification.IsValid() {
		f.LearningClassification = ClassificationForFinding(*f)
	}
	if strings.TrimSpace(f.LearningSignature) == "" {
		f.LearningSignature = FindingSignature(gate, *f)
	}
	if strings.TrimSpace(f.ID) == "" {
		sum := sha256.Sum256([]byte(f.LearningSignature))
		f.ID = "finding:sha256:" + hex.EncodeToString(sum[:])
	}
	if strings.TrimSpace(f.EvidenceDigest) == "" {
		f.EvidenceDigest = evidenceDigest
	}
}

// FindingSignature returns the normalized cross-run identity signature.
func FindingSignature(gate string, f apiv1.Finding) string {
	location := trailingLocation.ReplaceAllString(strings.TrimSpace(strings.ToLower(f.Location)), "")
	message := strings.Join(strings.Fields(strings.ToLower(f.Message)), " ")
	message = digitRun.ReplaceAllString(message, "#")
	class := string(f.LearningClassification)
	if class == "" {
		class = string(ClassificationForFinding(f))
	}
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(gate)),
		class,
		string(f.Class),
		string(f.Severity),
		location,
		message,
	}, "|")
}

// ClassificationForFinding derives the conservative governed action family.
func ClassificationForFinding(f apiv1.Finding) apiv1.LearningClassification {
	if f.LearningClassification.IsValid() {
		return f.LearningClassification
	}
	if f.Class == apiv1.FindingMissingTests {
		return apiv1.LearningValidation
	}
	if f.Class.RequiresCodeChange() {
		return apiv1.LearningCodeDefect
	}
	path := strings.ToLower(strings.ReplaceAll(f.Location, "\\", "/"))
	switch {
	case strings.Contains(path, "/skills/"):
		return apiv1.LearningSkill
	case strings.Contains(path, "/workflows/"):
		return apiv1.LearningWorkflow
	}
	return apiv1.LearningInstruction
}

// RecommendedAction maps a classification to its governed execution route.
func RecommendedAction(classification apiv1.LearningClassification) string {
	switch classification {
	case apiv1.LearningWorkflow, apiv1.LearningGate:
		return ActionWorkflowOrGate
	case apiv1.LearningValidation:
		return ActionTargetedTest
	case apiv1.LearningCodeDefect:
		return ActionCodeIssue
	default:
		return ActionInstructionOrSkill
	}
}

// ActionsForFindings builds the authoritative finding-level action contract.
func ActionsForFindings(findings []apiv1.Finding) []FindingAction {
	actions := make([]FindingAction, 0, len(findings))
	for _, finding := range findings {
		actions = append(actions, FindingAction{
			FindingID:         finding.ID,
			Signature:         finding.LearningSignature,
			Classification:    finding.LearningClassification,
			RecommendedAction: RecommendedAction(finding.LearningClassification),
			EvidenceDigest:    finding.EvidenceDigest,
		})
	}
	return actions
}

// EffectiveVersion identifies the exact workflow and goober digest pair.
func EffectiveVersion(workflowDigest, gooberDigest string) string {
	if workflowDigest == "" && gooberDigest == "" {
		return ""
	}
	data, _ := json.Marshal([]string{workflowDigest, gooberDigest})
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CombinedSignature returns a stable episode summary over finding signatures.
func CombinedSignature(findings []apiv1.Finding) string {
	signatures := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.LearningSignature != "" {
			signatures = append(signatures, finding.LearningSignature)
		}
	}
	sort.Strings(signatures)
	return strings.Join(signatures, "\n")
}

// EpisodeID returns a stable identity for one source event and finding set.
func EpisodeID(episode Episode) string {
	data, _ := json.Marshal(struct {
		RunID     string   `json:"runId"`
		SourceSeq uint64   `json:"sourceSeq"`
		Findings  []string `json:"findings"`
	}{
		RunID: episode.SourceRunID, SourceSeq: episode.SourceSeq,
		Findings: findingIDs(episode.Findings),
	})
	sum := sha256.Sum256(data)
	return "episode:sha256:" + hex.EncodeToString(sum[:])
}

func findingIDs(findings []apiv1.Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	sort.Strings(ids)
	return ids
}
