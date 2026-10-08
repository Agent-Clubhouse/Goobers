package readservice

import (
	"slices"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
)

// Reliability evidence and state vocabulary (#5313).
const (
	reliabilityUnknown = "unknown"

	reliabilityStateActive   = "active"
	reliabilityStateRetrying = "retrying"

	reliabilityEvidenceJournal       = "journal"
	reliabilityEvidenceTerminalCause = "terminalCause"

	reliabilityFailureNone         = "none"
	reliabilityFailureUnclassified = "unclassified"

	reliabilityRuleCauseClass      = "latestError.causes.class"
	reliabilityRuleErrorCode       = "latestError.code"
	reliabilityRuleTerminalReason  = "terminalReason"
	reliabilityRuleNoError         = "no-error-recorded"
	reliabilityRuleNoTerminalCause = "terminal-cause-not-recorded"
	reliabilityRuleTerminalCause   = "terminalCause.classification"
)

// Reliability budget kinds, in display order.
const (
	budgetImplementationReview = "implementation-review"
	budgetStagePolicy          = "stage-policy"
	budgetLocalInfra           = "local-infra"
	budgetLocalValidation      = "local-validation"
	budgetProviderRemediation  = "provider-remediation"
	budgetCIPoll               = "ci-poll"
)

// RunReliability is the implementation reliability projection shared by
// status and the dashboard (#5313). It is derived only from fields the run
// summary already projects from the journal or read model, so both read paths
// agree; anything those sources do not record is reported as unknown rather
// than inferred.
type RunReliability struct {
	// State is active, retrying, or the run's terminal phase.
	State string `json:"state"`
	// CurrentStage is the stage or gate the run is executing, if any.
	CurrentStage string `json:"currentStage,omitempty"`
	// CurrentAttempt is the active attempt number of CurrentStage; zero means
	// no attempt is active or the attempt is not recorded.
	CurrentAttempt int `json:"currentAttempt,omitempty"`
	// Failure is the normalized classification of the latest recorded failure.
	Failure ReliabilityFailure `json:"failure"`
	// Budgets reports consumed and remaining counts per retry target.
	Budgets []ReliabilityBudget `json:"budgets"`
	// LatestVerdict is the latest review verdict, or unknown when none is recorded.
	LatestVerdict string `json:"latestVerdict"`
	// Acceptance is the acceptance-criteria mapping state.
	Acceptance ReliabilityAcceptance `json:"acceptance"`
	// Retained lists the refs an operator can recover work from.
	Retained ReliabilityRetainedRefs `json:"retained"`
	// NextAction is what the system or an operator does next.
	NextAction string `json:"nextAction"`
	// HumanInterventionReason is the exact recorded reason an escalated run
	// needs a human, or unknown when the journal does not record one.
	HumanInterventionReason string `json:"humanInterventionReason,omitempty"`
}

// ReliabilityFailure is a failure classification and the evidence rule that
// produced it.
type ReliabilityFailure struct {
	// Classification is the normalized failure class, "unclassified" when an
	// error carries no class, or "none" when no failure is recorded.
	Classification string `json:"classification"`
	// EvidenceRule names the read-model field the classification came from.
	EvidenceRule string `json:"evidenceRule"`
	// Code is the latest error's stable code when one is recorded.
	Code string `json:"code,omitempty"`
}

// ReliabilityBudget is one retry target's consumed/remaining count. Nil counts
// are unknown: the journal does not record them for this target.
type ReliabilityBudget struct {
	// Kind is the retry target.
	Kind string `json:"kind"`
	// Consumed is the number of attempts spent on this target, nil when unknown.
	Consumed *int `json:"consumed"`
	// Remaining is the number of attempts left, nil when unknown.
	Remaining *int `json:"remaining"`
	// Evidence is "journal" when Consumed is the run-wide count projected from
	// journal events, "terminalCause" when both counts come from the recorded
	// terminal cause (scoped to its selector), otherwise "unknown".
	Evidence string `json:"evidence"`
}

// ReliabilityAcceptance is the acceptance-criteria mapping state.
type ReliabilityAcceptance struct {
	// State is the mapping state, unknown when no standard source records it.
	State string `json:"state"`
	// Digest identifies the mapping artifact when one is recorded.
	Digest string `json:"digest,omitempty"`
}

// ReliabilityRetainedRefs are the durable refs work can be recovered from.
type ReliabilityRetainedRefs struct {
	// Branch is the retained workspace branch.
	Branch string `json:"branch,omitempty"`
	// BranchSHA is the retained workspace branch head.
	BranchSHA string `json:"branchSha,omitempty"`
	// PullRequest is the PR this run opened or touched.
	PullRequest *journal.ExternalRef `json:"pullRequest,omitempty"`
	// RecoveryRunID is the run this run continues or resumed from.
	RecoveryRunID string `json:"recoveryRunId,omitempty"`
}

func withRunReliability(summary RunSummary) RunSummary {
	reliability := projectRunReliability(summary)
	summary.Operator.Reliability = &reliability
	return summary
}

func projectRunReliability(summary RunSummary) RunReliability {
	attempt := currentAttempt(summary)
	state := string(summary.Phase)
	if summary.Phase == journal.PhaseRunning {
		state = reliabilityStateActive
		if len(summary.RetryBackoff.Waits) > 0 || attempt > 1 {
			state = reliabilityStateRetrying
		}
	}
	out := RunReliability{
		State:          state,
		CurrentStage:   summary.Operator.CurrentStage,
		CurrentAttempt: attempt,
		Failure:        reliabilityFailure(summary),
		Budgets:        reliabilityBudgets(summary),
		LatestVerdict:  reliabilityUnknown,
		Acceptance:     ReliabilityAcceptance{State: reliabilityUnknown},
		Retained:       retainedRefs(summary),
		NextAction:     reliabilityNextAction(summary, state),
	}
	if summary.Operator.Review != nil && summary.Operator.Review.Verdict != "" {
		out.LatestVerdict = summary.Operator.Review.Verdict
	}
	if summary.Phase == journal.PhaseEscalated {
		out.HumanInterventionReason = summary.TerminalReason
		if out.HumanInterventionReason == "" {
			out.HumanInterventionReason = reliabilityUnknown
		}
	}
	return out
}

func currentAttempt(summary RunSummary) int {
	stage := summary.Operator.CurrentStage
	if stage == "" {
		return 0
	}
	for _, active := range summary.ActiveStages {
		if active.Name == stage {
			return active.Attempt
		}
	}
	return 0
}

func reliabilityFailure(summary RunSummary) ReliabilityFailure {
	if latest := summary.Operator.LatestError; latest != nil {
		for _, cause := range latest.Causes {
			if class := strings.ToLower(strings.TrimSpace(cause.Class)); class != "" {
				return ReliabilityFailure{Classification: class, EvidenceRule: reliabilityRuleCauseClass, Code: latest.Code}
			}
		}
		if latest.Code != "" {
			return ReliabilityFailure{Classification: reliabilityFailureUnclassified, EvidenceRule: reliabilityRuleErrorCode, Code: latest.Code}
		}
	}
	if summary.TerminalReason != "" {
		return ReliabilityFailure{Classification: reliabilityFailureUnclassified, EvidenceRule: reliabilityRuleTerminalReason}
	}
	if summary.Terminal && summary.Phase != journal.PhaseCompleted {
		// A non-completed terminal run ended for some reason; a source that
		// did not record it must not claim there was no failure.
		return ReliabilityFailure{Classification: reliabilityUnknown, EvidenceRule: reliabilityRuleNoTerminalCause}
	}
	return ReliabilityFailure{Classification: reliabilityFailureNone, EvidenceRule: reliabilityRuleNoError}
}

// reliabilityBudgets reports journal-measured consumption where a standard
// attempt counter exists. No read source records per-target limits yet, so
// every Remaining is unknown rather than guessed from configuration that may
// have changed since the run started.
func reliabilityBudgets(summary RunSummary) []ReliabilityBudget {
	measured := func(kind string, consumed int) ReliabilityBudget {
		return ReliabilityBudget{Kind: kind, Consumed: &consumed, Evidence: reliabilityEvidenceJournal}
	}
	unknown := func(kind string) ReliabilityBudget {
		return ReliabilityBudget{Kind: kind, Evidence: reliabilityUnknown}
	}
	return []ReliabilityBudget{
		measured(budgetImplementationReview, summary.RepassCount),
		measured(budgetStagePolicy, summary.PolicyRetryCount),
		measured(budgetLocalInfra, summary.InfraRetryCount),
		unknown(budgetLocalValidation),
		unknown(budgetProviderRemediation),
		unknown(budgetCIPoll),
	}
}

func retainedRefs(summary RunSummary) ReliabilityRetainedRefs {
	refs := ReliabilityRetainedRefs{RecoveryRunID: summary.Operator.ResumedFromRunID}
	if summary.Operator.PullRequest != nil {
		pr := *summary.Operator.PullRequest
		refs.PullRequest = &pr
	}
	if summary.Lineage != nil {
		refs.Branch = summary.Lineage.WorkspaceBranch
		refs.BranchSHA = summary.Lineage.WorkspaceBranchSHA
		if refs.RecoveryRunID == "" && summary.Lineage.Source != nil {
			refs.RecoveryRunID = summary.Lineage.Source.ID
		}
	}
	return refs
}

func reliabilityNextAction(summary RunSummary, state string) string {
	switch summary.Phase {
	case journal.PhaseRunning:
		if state == reliabilityStateRetrying && len(summary.RetryBackoff.Waits) > 0 {
			return "wait for retry backoff on " + summary.RetryBackoff.Waits[0].Stage
		}
		if summary.Operator.NextTransition != "" {
			return summary.Operator.NextTransition
		}
		return reliabilityUnknown
	case journal.PhaseEscalated:
		return "human intervention required"
	case journal.PhaseFailed:
		return "inspect the failure, then resume or rerun"
	default:
		return "none"
	}
}

// StatusLine renders the projection as one compact status line. Unknown
// counts render as "?" so missing evidence is never shown as zero.
func (r RunReliability) StatusLine() string {
	var b strings.Builder
	b.WriteString(r.State)
	if r.CurrentStage != "" {
		b.WriteString(" " + r.CurrentStage)
		if r.CurrentAttempt > 0 {
			b.WriteString(" attempt " + strconv.Itoa(r.CurrentAttempt))
		}
	}
	b.WriteString("; failure " + r.Failure.Classification + " (" + r.Failure.EvidenceRule + ")")
	budgets := make([]string, len(r.Budgets))
	for i, budget := range r.Budgets {
		budgets[i] = budget.Kind + " " + countOrUnknown(budget.Consumed) + " used/" + countOrUnknown(budget.Remaining) + " left"
	}
	b.WriteString("; budgets " + strings.Join(budgets, ", "))
	b.WriteString("; verdict " + r.LatestVerdict + "; acceptance " + r.Acceptance.State)
	b.WriteString("; next " + r.NextAction)
	if r.HumanInterventionReason != "" {
		b.WriteString("; needs human: " + r.HumanInterventionReason)
	}
	return b.String()
}

func countOrUnknown(count *int) string {
	if count == nil {
		return "?"
	}
	return strconv.Itoa(*count)
}

// withTerminalCauseReliability refines the projection with a durably recorded
// terminal cause, which carries the authoritative classification and the
// selector's configured allowances pinned at run start. Legacy causes rebuilt
// from log text are ignored so heuristics never look authoritative.
func withTerminalCauseReliability(summary RunSummary, cause *EscalationCause) RunSummary {
	if cause == nil || cause.Record == nil || summary.Operator.Reliability == nil {
		return summary
	}
	record := cause.Record
	reliability := *summary.Operator.Reliability
	reliability.Failure = ReliabilityFailure{
		Classification: string(record.Classification),
		EvidenceRule:   reliabilityRuleTerminalCause,
		Code:           record.Code,
	}
	reliability.Budgets = slices.Clone(reliability.Budgets)
	repassKind := budgetImplementationReview
	if record.Code == runcontrol.ReasonInfrastructureBudgetExhausted {
		repassKind = budgetLocalInfra
	}
	applyTerminalBudget(reliability.Budgets, repassKind, record.Repass)
	applyTerminalBudget(reliability.Budgets, budgetStagePolicy, record.Retry)
	applyTerminalBudget(reliability.Budgets, budgetCIPoll, record.Poll)
	summary.Operator.Reliability = &reliability
	return summary
}

// applyTerminalBudget replaces a budget with the terminal record's
// selector-scoped count. Remaining stays unknown when the record carries no
// allowance (a historical journal without its pinned definition).
func applyTerminalBudget(budgets []ReliabilityBudget, kind string, budget *journal.TerminalBudget) {
	if budget == nil {
		return
	}
	for i := range budgets {
		if budgets[i].Kind != kind {
			continue
		}
		consumed := budget.Consumed
		budgets[i] = ReliabilityBudget{Kind: kind, Consumed: &consumed, Evidence: reliabilityEvidenceTerminalCause}
		if budget.Allowed >= budget.Consumed {
			remaining := max(0, budget.Allowed-budget.Consumed)
			budgets[i].Remaining = &remaining
		}
		return
	}
}
