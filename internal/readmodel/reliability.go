package readmodel

import (
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// Stage-result conventions the reliability projection reads (#5313). A stage
// reports acceptance-criteria mapping evidence through scalar outputs, or by
// recording an artifact under AcceptanceMappingArtifact; a PR-opening stage
// reports whether the PR it opened is a draft alongside its prNumber.
const (
	AcceptanceMappingOutput       = "acceptanceMapping"
	AcceptanceMappingDigestOutput = "acceptanceMappingDigest"
	AcceptanceMappingArtifact     = "acceptance-mapping"
	PullRequestDraftOutput        = "draft"
	PullRequestNumberOutput       = "prNumber"

	// AcceptanceStateRecorded is reported when a mapping artifact is recorded
	// without a stage-reported state.
	AcceptanceStateRecorded = "recorded"
)

// ReliabilityFacts are the journal facts the implementation reliability
// projection needs beyond the run summary's own fields. Both read paths fold
// them with After so list views agree with run detail.
type ReliabilityFacts struct {
	// TerminalCause is the durably recorded cause of the current terminal
	// generation; nil while running, after a resume, or for legacy journals.
	TerminalCause *journal.TerminalCause `json:",omitempty"`
	// PullRequestDraft is the draft state a stage reported for the PR it opened.
	PullRequestDraft *PullRequestDraft `json:",omitempty"`
	// AcceptanceState is the latest stage-reported acceptance mapping state.
	AcceptanceState string `json:",omitempty"`
	// AcceptanceDigest identifies the latest acceptance mapping artifact.
	AcceptanceDigest string `json:",omitempty"`
}

// PullRequestDraft is a stage-reported draft state for one pull request.
type PullRequestDraft struct {
	ID    string
	Draft bool
}

// After folds one journal event into the facts.
func (f ReliabilityFacts) After(event journal.Event) ReliabilityFacts {
	if !event.KnownSchema() {
		return f
	}
	switch event.Type {
	case journal.EventRunFinished:
		f.TerminalCause = nil
		if event.TerminalCause != nil && event.TerminalCause.Schema == journal.TerminalCauseSchema {
			f.TerminalCause = cloneTerminalCause(*event.TerminalCause)
		}
	case journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested:
		f.TerminalCause = nil
	case journal.EventStageFinished:
		f = f.afterStageOutputs(event.Outputs)
	case journal.EventArtifactRecorded:
		if event.Name == AcceptanceMappingArtifact && event.Ref != nil && event.Ref.Digest != "" {
			f.AcceptanceDigest = event.Ref.Digest
			if f.AcceptanceState == "" {
				f.AcceptanceState = AcceptanceStateRecorded
			}
		}
	}
	return f
}

func (f ReliabilityFacts) afterStageOutputs(outputs map[string]any) ReliabilityFacts {
	if state, ok := outputs[AcceptanceMappingOutput].(string); ok && strings.TrimSpace(state) != "" {
		f.AcceptanceState = strings.ToLower(strings.TrimSpace(state))
		// A newly reported state supersedes any earlier artifact's digest
		// unless this result names its own.
		f.AcceptanceDigest = ""
	}
	if digest, ok := outputs[AcceptanceMappingDigestOutput].(string); ok && strings.TrimSpace(digest) != "" {
		f.AcceptanceDigest = strings.TrimSpace(digest)
		if f.AcceptanceState == "" {
			f.AcceptanceState = AcceptanceStateRecorded
		}
	}
	id, idOK := outputScalarString(outputs[PullRequestNumberOutput])
	draft, draftOK := outputBool(outputs[PullRequestDraftOutput])
	if idOK && draftOK {
		f.PullRequestDraft = &PullRequestDraft{ID: id, Draft: draft}
	}
	return f
}

func cloneTerminalCause(cause journal.TerminalCause) *journal.TerminalCause {
	for _, budget := range []**journal.TerminalBudget{&cause.Retry, &cause.Poll, &cause.Repass} {
		if *budget != nil {
			copy := **budget
			*budget = &copy
		}
	}
	return &cause
}

func outputScalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		return v, v != ""
	case float64:
		if v > 0 && v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
	case int:
		if v > 0 {
			return strconv.Itoa(v), true
		}
	}
	return "", false
}

func outputBool(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return parsed, err == nil
	}
	return false, false
}
