package readmodel

import (
	"cmp"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// TerminalExternalRefLimit bounds the per-run result projection. Raw journal
// references are retained independently; truncation never changes retention.
const TerminalExternalRefLimit = 16

// WorkflowOutcome is the #851 business-decision axis, separate from execution
// health. An empty outcome denotes a completed path with no gate decision.
type WorkflowOutcome struct {
	Gate           string `json:"gate,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	Target         string `json:"target,omitempty"`
	CausalEventSeq uint64 `json:"causalEventSeq,omitempty"`
}

// TerminalReason records provenance so legacy error evidence never masquerades
// as a durable terminal-cause classification.
type TerminalReason struct {
	Source         string                         `json:"source"`
	Classification journal.TerminalClassification `json:"classification,omitempty"`
	Code           string                         `json:"code,omitempty"`
	Message        string                         `json:"message,omitempty"`
	CausalEventSeq uint64                         `json:"causalEventSeq,omitempty"`
}

// TerminalExternalResult is a structured reference and the operation that
// selected it. ConfirmedMerge requires the provider's positive merge receipt;
// operation=merge alone can describe an observed, previously merged PR.
type TerminalExternalResult struct {
	Ref            journal.ExternalRef `json:"ref"`
	Operation      string              `json:"operation,omitempty"`
	ConfirmedMerge bool                `json:"confirmedMerge,omitempty"`
	CausalEventSeq uint64              `json:"causalEventSeq,omitempty"`
}

// TerminalSummary composes execution, workflow decision, useful-work evidence,
// cause and external results without changing the existing accounting buckets.
type TerminalSummary struct {
	Phase                 journal.RunPhase         `json:"phase"`
	Outcome               *WorkflowOutcome         `json:"outcome,omitempty"`
	WorkDisposition       string                   `json:"workDisposition"`
	Reason                *TerminalReason          `json:"reason,omitempty"`
	CausalEventSeq        uint64                   `json:"causalEventSeq,omitempty"`
	PrimaryExternalResult *TerminalExternalResult  `json:"primaryExternalResult,omitempty"`
	RelatedExternalRefs   []TerminalExternalResult `json:"relatedExternalRefs,omitempty"`
	RefsTruncated         bool                     `json:"refsTruncated,omitempty"`
	// Status is complete, partial or unavailable. Complete requires a durable
	// finish, known work disposition, and (for adverse phases) a typed cause.
	Status string `json:"status"`
	Legacy bool   `json:"legacy"`
}

// TerminalFacts is a bounded, incremental fold stored in OperatorFacts. The
// same fold serves journal detail and persisted list/telemetry readers.
type TerminalFacts struct {
	Outcome       *WorkflowOutcome
	Error         *TerminalReason
	ErrorStage    string
	ErrorBranch   int
	PendingError  *TerminalReason
	Refs          []TerminalExternalResult
	RefsTruncated bool
	Finished      bool
	FinishSeq     uint64
	FinishPhase   journal.RunPhase
	Disposition   string
	Produced      bool
	Steps         int // saturates at two; only a first-step no-work is semantic
	NoWorkSeq     uint64
}

// After folds one event without mutating the previous value. Resume and gate
// override reset lifecycle evidence; rerun resets terminal facts but preserves
// #851's existing last-gate boundary (resume/override).
func (f TerminalFacts) After(event journal.Event) TerminalFacts {
	if !event.KnownSchema() {
		return f
	}
	switch event.Type {
	case journal.EventRunResumed, journal.EventGateOverridden:
		f = TerminalFacts{}
	case journal.EventStageRerunRequested:
		f = TerminalFacts{Outcome: f.Outcome}
	}
	// Cleanup and notification errors after run.finished cannot replace its
	// cause. Late successful receipt custody copies may still add results.
	if f.Finished && event.Type == journal.EventRunFinished {
		// Cancellation can supersede escalation without a resume. Only then
		// may an intervening error explain a new terminal decision.
		if f.FinishPhase != journal.RunPhase(event.Status) {
			f.Error = f.PendingError
			f.ErrorStage, f.ErrorBranch = "", 0
		}
		f.Finished, f.PendingError = false, nil
	}
	if !f.Finished {
		f = f.observeExecution(event)
	} else if event.Type == journal.EventError && event.Error != nil {
		f.PendingError = &TerminalReason{Source: "error", Code: event.Error.Code, Message: event.Error.Message, CausalEventSeq: event.Seq}
	}
	if event.IsReferenceTouch() && event.Error == nil {
		outcome, _ := event.Runner["outcome"].(string)
		if outcome != "failure" && outcome != "failed" && outcome != "conflict" && outcome != "contention" {
			f = f.observeReference(event)
		}
	}
	return f
}

func (f TerminalFacts) observeExecution(event journal.Event) TerminalFacts {
	switch event.Type {
	case journal.EventStageStarted:
		f.clearRecoveredFailure(event)
		f.Steps = min(2, f.Steps+1)
	case journal.EventGateStarted, journal.EventParallelStarted, journal.EventMapStarted:
		f.Steps = min(2, f.Steps+1)
	case journal.EventStageFinished:
		f.Produced = f.Produced || event.Status == "success"
		if event.Status == "success" {
			f.clearRecoveredFailure(event)
		}
		if event.Status == DispositionNoWork {
			f.NoWorkSeq = event.Seq
		}
	case journal.EventGateEvaluated, journal.EventGateOverridden:
		f.Outcome = &WorkflowOutcome{Gate: event.Gate, Verdict: event.Verdict, Target: event.Target, CausalEventSeq: event.Seq}
	case journal.EventRunFinished:
		f.Finished, f.FinishSeq, f.Disposition = true, event.Seq, event.Disposition
		f.FinishPhase = journal.RunPhase(event.Status)
	case journal.EventError:
		if event.Error != nil && event.Error.Code == "executor_error" {
			// A failure before stage.started is still an attempted step.
			f.Steps = 2
		}
	}
	if event.Error != nil && (event.Type == journal.EventStageFinished || event.Type == journal.EventError || event.Type == journal.EventRunFinished) {
		source := "error"
		if event.Type == journal.EventStageFinished {
			source = "stage-error"
		}
		// Prefer a terminal stage's coded result over ancillary cleanup errors.
		if f.Error == nil || f.Error.Source != "stage-error" || source == "stage-error" {
			f.Error = &TerminalReason{Source: source, Code: event.Error.Code, Message: event.Error.Message, CausalEventSeq: event.Seq}
			f.ErrorStage, f.ErrorBranch = event.Stage, event.Branch
		}
	}
	return f
}

func (f *TerminalFacts) clearRecoveredFailure(event journal.Event) {
	if f.ErrorStage == event.Stage && f.ErrorBranch == event.Branch {
		f.Error, f.ErrorStage, f.ErrorBranch = nil, "", 0
	}
}

func (f TerminalFacts) observeReference(event journal.Event) TerminalFacts {
	operation, _ := event.Runner["operation"].(string)
	result := TerminalExternalResult{Ref: *event.ExternalRef, Operation: operation, CausalEventSeq: event.Seq}
	result.ConfirmedMerge = operation == "merge" && result.Ref.Kind == "pr" && confirmedTerminalMerge(event)
	if result.ConfirmedMerge || result.Ref.Kind == "pr" && operation == "open" ||
		(result.Ref.Kind == "commit" || result.Ref.Kind == "branch") && operation == "push" {
		f.Produced = true
	}
	f.Refs = slices.Clone(f.Refs)
	for i, prior := range f.Refs {
		if prior.Ref.Provider == result.Ref.Provider && prior.Ref.Kind == result.Ref.Kind && prior.Ref.ID == result.Ref.ID && prior.Ref.URL == result.Ref.URL {
			if compareTerminalRefs(result, prior) < 0 {
				f.Refs[i] = result
			}
			slices.SortFunc(f.Refs, compareTerminalRefs)
			return f
		}
	}
	f.Refs = append(f.Refs, result)
	slices.SortFunc(f.Refs, compareTerminalRefs)
	if len(f.Refs) > TerminalExternalRefLimit {
		f.Refs = f.Refs[:TerminalExternalRefLimit:TerminalExternalRefLimit]
		f.RefsTruncated = true
	}
	return f
}

func confirmedTerminalMerge(event journal.Event) bool {
	if event.ExternalRef.Provider != "github" && event.ExternalRef.Provider != "gitea" && event.ExternalRef.Provider != "ado" {
		return false
	}
	var confirmation providers.MergeConfirmation
	data, err := json.Marshal(event.Runner["mergeConfirmation"])
	if err != nil || json.Unmarshal(data, &confirmation) != nil || confirmation.PullID != event.ExternalRef.ID ||
		len(confirmation.PullID) == 0 || len(confirmation.PullID) > 32 || confirmation.PullID[0] == '0' || len(confirmation.MergeSHA) > 128 {
		return false
	}
	for _, digit := range confirmation.PullID {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return terminalMergeRepository(confirmation.RepositoryAPIURL)
}

func terminalMergeRepository(address string) bool {
	u, err := url.Parse(address)
	return len(address) <= 4096 && err == nil && u.Host != "" && u.Path != "" && u.User == nil && u.RawQuery == "" &&
		!u.ForceQuery && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Host == strings.ToLower(u.Host) && !strings.HasSuffix(u.Path, "/") && u.String() == address
}

func terminalRefRank(ref TerminalExternalResult) int {
	switch {
	case ref.ConfirmedMerge:
		return 0
	case ref.Ref.Kind == "pr" && ref.Operation == "open":
		return 1
	case (ref.Ref.Kind == "commit" || ref.Ref.Kind == "branch") && ref.Operation == "push":
		return 2
	case ref.Ref.Kind == "pr":
		return 3
	case ref.Ref.Kind == "commit":
		return 4
	case ref.Ref.Kind == "issue" && ref.Operation == "claim":
		return 5
	case ref.Ref.Kind == "issue":
		return 6
	default:
		return 7
	}
}

func compareTerminalRefs(a, b TerminalExternalResult) int {
	return cmp.Or(cmp.Compare(terminalRefRank(a), terminalRefRank(b)),
		cmp.Compare(b.CausalEventSeq, a.CausalEventSeq), cmp.Compare(a.Ref.Provider, b.Ref.Provider),
		cmp.Compare(a.Ref.Kind, b.Ref.Kind), cmp.Compare(a.Ref.ID, b.Ref.ID),
		cmp.Compare(a.Ref.URL, b.Ref.URL), cmp.Compare(a.Operation, b.Operation))
}

// Summary derives the same contract for every read surface. A zero facts value
// explicitly reports unavailable rather than treating a rollup status or the
// historical produced accounting bucket as evidence of useful work.
func (f TerminalFacts) Summary(phase journal.RunPhase, cause *journal.TerminalCause) *TerminalSummary {
	if phase == "" || phase == journal.PhaseRunning {
		return nil
	}
	summary := &TerminalSummary{Phase: phase, WorkDisposition: f.workDisposition(phase), Status: "unavailable", Legacy: true}
	if f.Finished {
		summary.Status = "partial"
		summary.CausalEventSeq = f.FinishSeq
	}
	if phase == journal.PhaseCompleted {
		summary.Outcome = &WorkflowOutcome{}
		if f.Outcome != nil {
			*summary.Outcome = *f.Outcome
			summary.CausalEventSeq = f.Outcome.CausalEventSeq
		}
	} else if cause != nil && cause.Schema == journal.TerminalCauseSchema && cause.Phase == phase {
		summary.Reason = &TerminalReason{Source: "terminal-cause", Classification: cause.Classification,
			Code: cause.Code, Message: cause.Message, CausalEventSeq: cause.CausalEventSeq}
	} else if f.Error != nil {
		reason := *f.Error
		summary.Reason = &reason
	}
	if summary.Reason != nil {
		summary.CausalEventSeq = summary.Reason.CausalEventSeq
	}
	if len(f.Refs) > 0 {
		primary := f.Refs[0]
		summary.PrimaryExternalResult = &primary
		summary.RelatedExternalRefs = slices.Clone(f.Refs[1:])
	}
	summary.RefsTruncated = f.RefsTruncated
	f.completeSummary(summary)
	return summary
}

func (f TerminalFacts) workDisposition(phase journal.RunPhase) string {
	legacyNoWork := f.Disposition == "" && f.Steps == 1 && f.NoWorkSeq != 0 && !f.Produced && f.Outcome == nil
	if phase == journal.PhaseCompleted && (f.Disposition == DispositionNoWork || legacyNoWork) {
		return DispositionNoWork
	}
	if f.Produced {
		return DispositionProduced
	}
	return DispositionUnknown
}

func (f TerminalFacts) completeSummary(summary *TerminalSummary) {
	if summary.WorkDisposition != DispositionUnknown || f.Outcome != nil || summary.Reason != nil || len(f.Refs) > 0 {
		summary.Status = "partial"
	}
	summary.Legacy = !f.Finished || f.Disposition == "" || summary.Phase != journal.PhaseCompleted && (summary.Reason == nil || summary.Reason.Source != "terminal-cause")
	if !summary.Legacy && summary.WorkDisposition != DispositionUnknown && !summary.RefsTruncated {
		summary.Status = "complete"
	}
}
