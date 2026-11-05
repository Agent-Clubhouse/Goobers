package runsemantics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

// DefaultMaxSteps bounds the state walk against a runaway machine (carried over
// from the Temporal engine core, ARCHITECTURE §3.1).
const DefaultMaxSteps = 10000

// DefaultMaxInfrastructureAttempts bounds transient infrastructure failures
// independently of a task's policy retry allowance.
const DefaultMaxInfrastructureAttempts int32 = runcontrol.DefaultMaxInfrastructureAttempts

func remediationFailureEvidencePointers(cause *gate.RepassCause, pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	if cause == nil {
		return nil
	}
	var prefix string
	switch cause.Kind {
	case "reviewer":
		if cause.Gate == "" {
			return nil
		}
		prefix = cause.Gate + ".verdict"
	case "stage-failure":
		if cause.Stage == "" {
			return nil
		}
		prefix = cause.Stage + ".artifact["
	default:
		return nil
	}
	var required []apiv1.ContextPointer
	for _, pointer := range pointers {
		if pointer.Name == prefix || (strings.HasSuffix(prefix, "[") && strings.HasPrefix(pointer.Name, prefix)) {
			required = append(required, pointer)
		}
	}
	return required
}

// ActionableEvidence is one required failure-evidence pointer's obligation.
// Exported alongside RemediationEvidenceRequirements; actionableEvidence stays
// as the in-package name so every existing reference and every recorded
// annotation payload are unchanged.
type ActionableEvidence struct {
	Pointer    string             `json:"pointer"`
	Ranges     []ReceiptLineRange `json:"ranges,omitempty"`
	Signatures []string           `json:"signatures,omitempty"`
}

// RemediationEvidenceRequirements derives, for each required failure-evidence
// pointer, the check annotations a remediation attempt is obliged to have
// actually read: the normalized signatures, and the line ranges those
// signatures occupy in the evidence artifact.
//
// This IS the #3375 obligation. The receipt check on the far side compares an
// attempt's claimed reads against these entries, so it decides whether a
// remediation gets re-dispatched or escalates the run. Exported for #3882 so
// the engine derives the identical obligation from the identical bytes rather
// than a lookalike: two derivations here would mean the two lanes demand
// different reads of the same failure.
func RemediationEvidenceRequirements(
	pointers []apiv1.ContextPointer,
	resolve gate.ArtifactBytes,
) []ActionableEvidence {
	requirements := make([]ActionableEvidence, 0, len(pointers))
	for _, pointer := range pointers {
		requirement := ActionableEvidence{Pointer: pointer.Name}
		if pointer.Artifact == nil || resolve == nil {
			requirements = append(requirements, requirement)
			continue
		}
		data, err := resolve(*pointer.Artifact)
		if err != nil {
			requirements = append(requirements, requirement)
			continue
		}
		var evidence struct {
			Checks []struct {
				Annotations []struct {
					Path      string `json:"path"`
					StartLine int    `json:"startLine"`
					EndLine   int    `json:"endLine"`
					Title     string `json:"title"`
					Message   string `json:"message"`
				} `json:"annotations"`
			} `json:"checks"`
		}
		if json.Unmarshal(data, &evidence) != nil {
			requirements = append(requirements, requirement)
			continue
		}
		for _, check := range evidence.Checks {
			for _, annotation := range check.Annotations {
				signature := normalizeEvidenceSignature(annotation.Path, annotation.Title, annotation.Message)
				if signature == "" {
					continue
				}
				requirement.Signatures = append(requirement.Signatures, signature)
				start := bytes.Index(data, []byte(annotation.Message))
				if start >= 0 {
					line := bytes.Count(data[:start], []byte{'\n'}) + 1
					requirement.Ranges = append(requirement.Ranges, receiptLineRange{Start: line, End: line})
				}
			}
		}
		requirements = append(requirements, requirement)
	}
	return requirements
}

func normalizeEvidenceSignature(parts ...string) string {
	words := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Join(strings.Fields(strings.ToLower(part)), " ")
		if part != "" {
			words = append(words, part)
		}
	}
	return strings.Join(words, " ")
}

// BaseSyncConflictErrorCode is the stage-failure code a syncBase base-merge
// conflict surfaces under (#813). Exported so the Temporal engine's activity
// host reports the identical normative error code instead of a string copy
// that can drift (the #624 shared-constant pattern).
const BaseSyncConflictErrorCode = "base_sync_conflict"

// WorkspaceHeadMovedErrorCode is the stage-failure code a pod reports when
// the workspace-delta ancestry guard refuses to land this run's delta because
// the branch the run REBOUND its workspace to (a pull request's own head,
// #392) moved under the run: someone pushed to the PR while the run was
// working (#5948). The refusal is correct — landing the delta would discard
// that push — but it is not a failure of the run's work: the subject moved,
// so the work is moot. The engine ends such a run completed (superseded),
// never failed, and the terminal handlers neither charge nor credit the
// claimed item for it (#6019). Exported for the same reason
// BaseSyncConflictErrorCode is (the #624 shared-constant pattern): the pod
// entrypoint writes it and the engine routes on it.
const WorkspaceHeadMovedErrorCode = "workspace_head_moved"

// RetryDecisionKind and RetryFailureClassKey are the runner.annotation shape
// routeRetryDecision writes when a gate's fail branch re-enters an
// already-completed stage, and priorRepassCause reads back to tell an
// infrastructure repass from a content one. Exported for the same reason
// BaseSyncConflictErrorCode is (the #624 shared-constant pattern): the Temporal
// engine writes the identical annotation on its own gate repass route, and a
// string copy on that side would drift silently — the reader keys on the exact
// kind and class key, so a divergence is invisible until an infra repass is
// misclassified as content.
const (
	RetryDecisionKind    = "stage.retry.decision"
	RetryFailureClassKey = "retryFailureClass"
)

// Shared retry markers retain the existing journal field spellings.
const (
	InterruptedAttemptErrorCode = "interrupted"
	InterruptedAttemptMarkerKey = "interruptedAttempt"
	retryFailureClassKey        = RetryFailureClassKey
	InfraCommittedWorkKey       = "infraFailedAttemptCommittedWork"
	retryDecisionKind           = RetryDecisionKind
	ToleratedFailureErrorCode   = "stage_failure_tolerated"
	baseSyncConflictErrorCode   = BaseSyncConflictErrorCode
)

// RunnerInt decodes an integer from a journal annotation value.
func RunnerInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		value, err := n.Int64()
		return int(value), err == nil
	}
	return 0, false
}

// retryFailureClass identifies command and sync-conflict failures whose
// status-equals gate outcome is known without dispatching the checker.
func retryFailureClass(g apiv1.Gate, result apiv1.ResultEnvelope) (journal.AttemptClass, string, bool) {
	if g.Evaluator != apiv1.EvaluatorAutomated || g.Automated == nil || g.Automated.Check != "status-equals" {
		return "", "", false
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		return "", "", false
	}
	switch result.Error.Code {
	case "nonzero_exit", baseSyncConflictErrorCode:
		want := g.Automated.Params["equals"]
		if want == "" {
			want = string(apiv1.ResultSuccess)
		}
		outcome := gate.OutcomeFail
		if string(result.Status) == want {
			outcome = gate.OutcomePass
		}
		return journal.AttemptPolicy, outcome, true
	default:
		return "", "", false
	}
}

func retryFailureClassForGateResult(g apiv1.Gate, result apiv1.ResultEnvelope, outcome string) (journal.AttemptClass, string, bool) {
	class, knownOutcome, retryable := retryFailureClass(g, result)
	if !retryable && outcome == gate.OutcomeInfra {
		return journal.AttemptInfra, "", true
	}
	return class, knownOutcome, retryable
}

// RetryFailureClass exports retryFailureClass for the Temporal engine, which
// applies the identical known-outcome shortcut and retry-decision classification
// on its own gate arm. Shared rather than mirrored (the #624 shared-constant
// pattern) because the recognized code set (nonzero_exit / base_sync_conflict)
// and the status-equals default are runner-owned policy: a copy would decide to
// dispatch a checker on one runner and skip it on the other.
func RetryFailureClass(g apiv1.Gate, result apiv1.ResultEnvelope) (journal.AttemptClass, string, bool) {
	return retryFailureClass(g, result)
}

// RetryFailureClassForGateResult exports retryFailureClassForGateResult, which
// folds an infrastructure gate outcome into the same classification.
func RetryFailureClassForGateResult(g apiv1.Gate, result apiv1.ResultEnvelope, outcome string) (journal.AttemptClass, string, bool) {
	return retryFailureClassForGateResult(g, result, outcome)
}

// Unpushed-diff artifact names (#3366): the well-known per-stage artifact pair
// under which the runner persists a run branch's committed-but-not-yet-
// published work. The names are a discovery contract: cmd/goobers's
// gather-implement-context scans run journals for "*/unpushed-diff.json" to
// offer a stranded prior diff to the next run on the same backlog item.
const (
	unpushedDiffPatchName     = "unpushed-diff.patch"
	unpushedDiffMetaName      = "unpushed-diff.json"
	unpushedDiffSchemaVersion = "goobers.dev/unpushed-diff/v1"
)

// escalateErrorCodes are the recognized non-retryable business dispositions an
// agentic stage can emit to bypass the Next gate's repass loop (#415). Each
// names a conclusion that re-running the stage can only re-derive — the item
// needs a human or a future decomposition workflow, not another implement
// attempt. Kept as a runner-owned policy set (the runner owns status→transition
// routing), not a schema enum, so recognizing a new code never reopens the
// closed envelope contract.
//
// ISSUE_NOT_APPLICABLE (#3363) is an item judgment, not a work failure: the
// implementer verified the issue's premise no longer holds (targets deleted
// files, work already done). Before it was recognized here, that refusal
// routed into the Next gate, review-failed its empty diff, burned the repass
// budget re-deriving the identical conclusion, and escalated with the
// reasoning left only in the run journal.
var escalateErrorCodes = map[string]bool{
	"ISSUE_OVER_SCOPE":                  true,
	"NEEDS_DECOMPOSITION":               true,
	telemetry.ErrCodeIssueNotApplicable: true,
	// SHARED_BASELINE_FAILURE (#2971) is the same shape of conclusion reached
	// deterministically rather than by an agent: the target branch itself fails
	// the identical CI command at the pinned base SHA, so no repass on this
	// branch can fix it. The run parks against the shared blocker instead of
	// spending its remediation budget re-deriving that.
	SharedBaselineFailureCode: true,
}

// isNonRetryableEscalation reports whether a stage failure is a non-retryable
// business disposition the runner escalates on sight (#415): error.retryable
// is false AND error.code is a recognized escalate code. A failure that is
// retryable, or that carries an absent/unrecognized code, is not escalated
// here — it follows the ordinary failure route (into the Next gate, else
// PhaseFailed). nil in → false (no error detail, nothing to route on).
func isNonRetryableEscalation(e *apiv1.ErrorInfo) bool {
	return e != nil && !e.Retryable && escalateErrorCodes[e.Code]
}

// IsNonRetryableEscalation exports isNonRetryableEscalation for the Temporal
// engine's own #415 route (the #624 shared-constant pattern). escalateErrorCodes
// is runner-owned POLICY, not a schema enum, so a copied code set on the engine
// side would drift the moment a new disposition is recognized here — and the
// symptom would be an item silently re-entering the reviewer→implement loop on
// one runner and escalating on the other.
func IsNonRetryableEscalation(e *apiv1.ErrorInfo) bool {
	return isNonRetryableEscalation(e)
}

// TaskEscalationTarget lets a workflow intercept a non-retryable task
// disposition through the escalation control branch on its Next gate. The gate
// evaluator is deliberately bypassed; absent that control branch, the existing
// direct @escalate behavior remains unchanged.
func TaskEscalationTarget(machine *workflow.Machine, task apiv1.Task) string {
	if nextGate, ok := machine.Gate(task.Next); ok {
		if target, ok := workflow.BranchTarget(nextGate, workflow.BranchEscalate); ok {
			return target
		}
	}
	return workflow.TargetEscalate
}

// PriorRepassCause is the pure half: the backward scan over an already-read
// event log that classifies WHY a stage is being re-entered, given a way to
// resolve a gate.evaluated event's verdict artifact. The re-entry window starts
// after the subject's previous completion, so successful forward gates between
// a correction and the current subject cannot replace the originating cause.
//
// Exported and split from the reader above for #3882: the engine holds its
// journal as workflow state (a JournalProjection whose artifact bytes are
// inline), not as a directory it can journal.OpenRead, so the only thing the
// two lanes can share is this function over an event slice. The classification
// itself — reviewer rationale beats gate outcome, a preceding failed
// stage.finished beats both, and the infrastructure verdict comes from the
// retry-decision annotation rather than the attempt class — is normative: it
// becomes the RepassCause the reviewer sees, so a second implementation of it
// would let the two lanes hand the same reviewer different histories.
func PriorRepassCause(
	events []journal.Event,
	subjectStage string,
	artifactBytes func(journal.Ref) ([]byte, error),
) (*gate.RepassCause, error) {
	windowStart := priorSubjectCompletion(events, subjectStage)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventGateEvaluated || event.Verdict == gate.OutcomePass {
			continue
		}
		if event.Target != subjectStage && windowStart >= 0 && i <= windowStart {
			continue
		}
		cause := &gate.RepassCause{Kind: "gate", Gate: event.Gate, Outcome: event.Verdict}
		if event.Ref != nil {
			if artifactBytes == nil {
				return nil, fmt.Errorf("read prior verdict for gate %q: no artifact resolver", event.Gate)
			}
			data, err := artifactBytes(*event.Ref)
			if err != nil {
				return nil, fmt.Errorf("read prior verdict for gate %q: %w", event.Gate, err)
			}
			var verdict apiv1.Verdict
			if err := json.Unmarshal(data, &verdict); err != nil {
				return nil, fmt.Errorf("parse prior verdict for gate %q: %w", event.Gate, err)
			}
			cause.Kind = "reviewer"
			cause.Rationale = strings.TrimSpace(verdict.Rationale)
			if cause.Rationale == "" {
				cause.Rationale = strings.TrimSpace(verdict.Summary)
			}
			cause.Findings = append([]apiv1.Finding(nil), verdict.Findings...)
		}
		var infrastructureEvidence *bool
		for j := i + 1; j < len(events); j++ {
			following := events[j]
			if following.Type == journal.EventStageStarted ||
				following.Type == journal.EventStageFinished ||
				following.Type == journal.EventGateStarted ||
				following.Type == journal.EventGateEvaluated {
				break
			}
			if following.Type == journal.EventRunnerAnnotation &&
				following.Gate == event.Gate &&
				following.Runner["kind"] == retryDecisionKind &&
				following.Runner[retryFailureClassKey] == string(journal.AttemptInfra) {
				infrastructure := true
				infrastructureEvidence = &infrastructure
				break
			}
		}
		for j := i - 1; j >= 0; j-- {
			prior := events[j]
			if prior.Type == journal.EventGateEvaluated {
				break
			}
			switch {
			case prior.Type == journal.EventRunnerAnnotation &&
				prior.Runner[retryFailureClassKey] == string(journal.AttemptInfra):
				infrastructure := true
				infrastructureEvidence = &infrastructure
			case prior.Type == journal.EventError && prior.Stage == subjectStage &&
				prior.Runner[retryFailureClassKey] == string(journal.AttemptInfra):
				infrastructure := true
				infrastructureEvidence = &infrastructure
			}
			if prior.Type == journal.EventStageFinished && prior.Status == string(apiv1.ResultFailure) {
				cause.Kind = "stage-failure"
				cause.Stage = prior.Stage
				// AttemptClass describes how an attempt started, not why it
				// failed. Use the retry decision journaled for this stage.
				cause.Infrastructure = infrastructureEvidence != nil && *infrastructureEvidence
				if prior.Error != nil {
					cause.ErrorCode = prior.Error.Code
					cause.ErrorMessage = prior.Error.Message
				}
				break
			}
		}
		return cause, nil
	}
	return nil, nil
}

func priorSubjectCompletion(events []journal.Event, subjectStage string) int {
	seenCurrent := false
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventStageFinished || event.Stage != subjectStage {
			continue
		}
		if seenCurrent {
			return i
		}
		seenCurrent = true
	}
	return -1
}

// MachineUsesRepo reports whether any stage of the compiled workflow touches a
// repository workspace — the predicate behind lazy run-branch provenance.
// Exported so the Temporal engine's journal projection (#629) applies the
// identical rule instead of a copy that can drift (the #624 shared-constant
// pattern).
func MachineUsesRepo(machine *workflow.Machine) bool {
	return machineUsesRepo(machine)
}

func machineUsesRepo(machine *workflow.Machine) bool {
	for _, task := range machine.Def.Spec.Tasks {
		if task.Type == apiv1.TaskAgentic || task.Run == nil || task.Run.Workspace != apiv1.WorkspaceScratch {
			return true
		}
	}
	for _, g := range machine.Def.Spec.Gates {
		if g.Evaluator == apiv1.EvaluatorAgentic {
			return true
		}
	}
	return false
}
