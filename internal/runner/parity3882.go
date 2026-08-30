package runner

import (
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/workflow"
)

// This file is the SHARED half of decision 005's implementation-lane parity
// bundle (#3882), on the runner side: the artifact names, sidecar shapes,
// synthesized results, instruction addenda, and pure decision functions the
// Temporal engine's walk and activity host need in order to reproduce this
// runner's implementation-lane behaviour EXACTLY.
//
// Every export below follows the #624 shared-constant pattern already
// established by BaseSyncConflictErrorCode and RetryDecisionKind. The rule is
// the same one and it is worth restating, because this bundle is where it
// bites hardest: the values here are ARTIFACT NAMES and ARTIFACT BYTES. An
// artifact's name and content digest are conformance-normative fields of the
// artifact.recorded event (ARCHITECTURE §3.3, journal.ConformanceView), so a
// second copy of `unpushed-diff.json`'s field set on the engine side is not a
// tolerable duplication — it is a guaranteed cross-runner journal divergence
// the moment either copy gains a field. Worse, `unpushed-diff.json` is a
// DISCOVERY CONTRACT: cmd/goobers' gather-implement-context reads it to offer
// a stranded prior diff to the next run on the same item, so a drifted engine
// copy strands exactly the work the mechanism exists to rescue.
//
// Nothing here adds behaviour to the local runner. Each function is the code
// that was already inline at its one call site, lifted so a second caller
// cannot fork it.

// ReviewerDiffArtifactName / ReviewerDiffPointerName are the #301 reviewer
// diff-evidence names: the per-gate artifact holding `git diff base...HEAD` of
// the run branch, and the ContextPointer the reviewer's envelope carries it as.
//
// The POINTER name is load-bearing beyond naming: internal/gate's
// deterministic finding-disproval reads the reviewer's own patch back through
// exactly `<gate>.diff` (reviewerPatchSource), so a differently-named pointer
// on the engine silently disables disproval rather than failing.
func ReviewerDiffArtifactName(runID, gateName string) string {
	return runID + ":" + gateName + "/reviewer-diff.patch"
}

// ReviewerDiffPointerName is the ContextPointer name for the same evidence.
func ReviewerDiffPointerName(gateName string) string { return gateName + ".diff" }

// ReviewerDiffMediaType is the media type both runners stamp on the reviewer
// diff artifact. gate.isDiffPointer accepts it explicitly.
const ReviewerDiffMediaType = "text/x-diff"

// CachedVerdictOutputKey is the deterministic-subject output a stage uses to
// hand a gate a digest-matched prior verdict (#523). merge-review's
// gather-sibling-context is the only producer today.
const CachedVerdictOutputKey = "cachedVerdictJson"

// CachedVerdictFromOutputs decodes a deterministic subject stage's
// digest-matched prior verdict out of its Outputs (#523), or nil when there is
// none.
//
// Silence on a decode failure is deliberate and is the contract, not laziness:
// an absent or malformed cachedVerdictJson is exactly the normal "no cache
// hit" case for every gate that never produces this key at all — which is
// every gate but merge-review's review gate. Making it fatal would fail every
// other lane on a key they do not participate in.
func CachedVerdictFromOutputs(outputs map[string]interface{}) *apiv1.Verdict {
	raw, ok := outputs[CachedVerdictOutputKey].(string)
	if !ok || raw == "" {
		return nil
	}
	var v apiv1.Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return &v
}

// BaseSyncConflictArtifactName is the per-stage artifact a syncBase base-merge
// conflict records its detail under (#813).
func BaseSyncConflictArtifactName(stage string) string { return stage + "/base-sync-conflict.json" }

// BaseSyncConflictSummary is the stage summary both runners report for a
// routed base-sync conflict.
const BaseSyncConflictSummary = "base synchronization conflicted; the implementation branch was preserved for remediation"

// BaseSyncConflictDetail is the machine-readable conflict artifact's shape.
// The exported spelling of baseSyncConflictArtifact, which is kept as an alias
// so no second type exists (the engine records the identical bytes).
type BaseSyncConflictDetail struct {
	Code             string   `json:"code"`
	Message          string   `json:"message"`
	Branch           string   `json:"branch"`
	BaseRef          string   `json:"baseRef"`
	ConflictingFiles []string `json:"conflictingFiles"`
}

type baseSyncConflictArtifact = BaseSyncConflictDetail

// SalvageOnTimeoutArtifactName is the #724 provenance marker recorded when an
// agentic session timeout is salvaged from its already-committed diff.
func SalvageOnTimeoutArtifactName(stage string) string { return stage + "/salvage-on-timeout.json" }

// SalvagedOnTimeoutOutput is the output key a salvaged completion carries, and
// the marker field name inside SalvageOnTimeoutMarker's bytes.
const SalvagedOnTimeoutOutput = "salvagedOnTimeout"

// SalvageOnTimeoutSummary is the salvaged stage's summary on both runners.
const SalvageOnTimeoutSummary = "salvaged committed diff after agentic session timeout (#724); local-ci verifies it authoritatively"

// SalvageOnTimeoutMarker builds the provenance marker bytes for a salvaged
// timeout. Deliberately not the diff bytes: the reviewer gate recomputes and
// digests the diff downstream, so the marker only has to make a salvaged
// completion distinguishable in the journal from an ordinary one.
func SalvageOnTimeoutMarker(diffBytes int, cause string) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		SalvagedOnTimeoutOutput: true,
		"diffBytes":             diffBytes,
		"cause":                 cause,
	})
}

// SalvagedResult is the ResultEnvelope a salvaged timeout completes with.
func SalvagedResult() apiv1.ResultEnvelope {
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: SalvageOnTimeoutSummary,
		Outputs: map[string]interface{}{SalvagedOnTimeoutOutput: true},
	}
}

// UnpushedDiff artifact names and schema (#3366). Exported because the names
// are the cross-run DISCOVERY CONTRACT gather-implement-context scans for.
const (
	UnpushedDiffPatchName     = unpushedDiffPatchName
	UnpushedDiffMetaName      = unpushedDiffMetaName
	UnpushedDiffSchemaVersion = unpushedDiffSchemaVersion
)

// UnpushedDiffMetadata is the exported spelling of the discovery sidecar
// recorded beside the diff artifact. unpushedDiffMetadata is an alias, so the
// bytes both runners commit are produced by one struct definition.
//
// Every field must remain a deterministic function of the run's inputs — see
// unpushedDiffMetadata's own doc for why a timestamp here would break the
// cross-runner conformance comparison.
type UnpushedDiffMetadata struct {
	Schema    string                `json:"schema"`
	RunID     string                `json:"runId"`
	Workflow  string                `json:"workflow,omitempty"`
	Stage     string                `json:"stage"`
	Attempt   int                   `json:"attempt"`
	ItemIDs   []string              `json:"itemIds,omitempty"`
	ItemURL   string                `json:"itemUrl,omitempty"`
	Branch    string                `json:"branch,omitempty"`
	BaseRef   string                `json:"baseRef"`
	DiffBytes int                   `json:"diffBytes"`
	Diff      apiv1.ArtifactPointer `json:"diff"`
}

type unpushedDiffMetadata = UnpushedDiffMetadata

// UnpushedDiffPatchArtifactName / UnpushedDiffMetaArtifactName are the
// per-stage artifact names, spelled once.
func UnpushedDiffPatchArtifactName(stage string) string {
	return stage + "/" + UnpushedDiffPatchName
}

// UnpushedDiffMetaArtifactName names the discovery sidecar.
func UnpushedDiffMetaArtifactName(stage string) string {
	return stage + "/" + UnpushedDiffMetaName
}

// ContextNotInspectedCode is the validation code a blocked DEPENDENCY_NOT_MET
// result is rewritten to when the stage never inspected the context pointers
// it was handed. The walk routes on it: the stage is re-dispatched once with a
// corrective addendum rather than the run escalating on an unproven claim.
const ContextNotInspectedCode = "CONTEXT_NOT_INSPECTED"

// DependencyNotMetCode is the producer-reported blocked code the validation
// above applies to.
const DependencyNotMetCode = "DEPENDENCY_NOT_MET"

// ContextNotInspectedAddendum is the instruction addendum the re-dispatch
// carries. Spelled once so both runners hand the agent the same correction.
func ContextNotInspectedAddendum(message string) string {
	return "Your previous result was rejected by the runner: " + message +
		". Inspect every provided context pointer with list_inputs and read_input or grep_input before returning DEPENDENCY_NOT_MET."
}

// ValidateDependencyNotMetTranscript is the PURE half of the #3375-sibling
// DEPENDENCY_NOT_MET validation: given the agent's transcript bytes and the
// pointers its invocation carried, did it call list_inputs and read or grep
// every required pointer? nil means the claim is evidenced.
//
// The runner's own validateDependencyNotMet resolves the transcript from the
// run journal and then calls this; the engine's activity host resolves it from
// the worker's span store and calls the same function, so the two cannot
// disagree about what counts as inspection.
func ValidateDependencyNotMetTranscript(transcript []byte, requiredPointers []apiv1.ContextPointer, result apiv1.ResultEnvelope) *apiv1.ErrorInfo {
	requiredNames := requiredContextPointerNames(requiredPointers)
	requiredSet := make(map[string]struct{}, len(requiredNames))
	for _, name := range requiredNames {
		requiredSet[name] = struct{}{}
	}
	sawListInputs, inspected := parseTranscriptInputInspection(transcript, requiredSet)
	missing := make([]string, 0, len(requiredNames))
	for _, name := range requiredNames {
		if !inspected[name] {
			missing = append(missing, name)
		}
	}
	if sawListInputs && len(missing) == 0 {
		return nil
	}
	return &apiv1.ErrorInfo{
		Code:    ContextNotInspectedCode,
		Message: dependencyValidationMessage(pointerValidationErrorMessage(missing, sawListInputs), result),
	}
}

// AppliesDependencyValidation reports whether a result is the shape the
// DEPENDENCY_NOT_MET validation applies to: blocked, self-reported
// DEPENDENCY_NOT_MET, and dispatched with at least one context pointer to
// have inspected. Exported so the engine's activity applies the identical
// precondition rather than a re-derived one.
func AppliesDependencyValidation(result apiv1.ResultEnvelope, invocationPointers []apiv1.ContextPointer) bool {
	return result.Status == apiv1.ResultBlocked && result.Error != nil &&
		result.Error.Code == DependencyNotMetCode && len(invocationPointers) > 0
}

// RejectDependencyResult applies a validation failure to the result the way
// the local runner does: the error is replaced by the validation error and the
// unproven outputs/artifacts are discarded.
func RejectDependencyResult(result apiv1.ResultEnvelope, validationErr *apiv1.ErrorInfo) apiv1.ResultEnvelope {
	if validationErr == nil {
		return result
	}
	result.Error = validationErr
	result.Outputs = nil
	result.Artifacts = nil
	return result
}

// MaxRemediationEvidenceRejections is the #3375 rejection bound, exported so
// the engine's identical loop is bounded by the same number rather than by its
// own step ceiling.
const MaxRemediationEvidenceRejections = maxRemediationEvidenceRejections

// RemediationEvidenceRejectionAddendum is the corrective addendum a rejected
// unchanged remediation is re-dispatched with. Naming the rejection ordinal
// and the bound is part of the instruction: an agent that knows it has two
// tries left behaves differently from one that believes the loop is infinite.
func RemediationEvidenceRejectionAddendum(message string, rejection, bound int) string {
	return fmt.Sprintf(
		"Your unchanged remediation result was rejected by the runner: %s. Inspect every required "+
			"failure-evidence pointer with list_inputs and read_input or grep_input, then explain why the "+
			"failure is non-actionable if no source change is needed. This was rejection %d of %d — after "+
			"the last one this gate escalates the run instead of dispatching you again.",
		message, rejection, bound,
	)
}

// RemediationEvidenceRequiredKind / RemediationEvidenceValidationKind are the
// two runner.annotation kinds the #3375 evidence loop writes: the requirement
// published before the repass, and the rejection recorded when it was not met.
const (
	RemediationEvidenceRequiredKind   = "remediation-evidence-required"
	RemediationEvidenceValidationKind = "remediation-evidence-validation"
)

// RemediationFailureEvidencePointers selects, from the pointers a repassed
// stage will be handed, the ones that ARE the failure evidence its repass has
// to inspect — the triggering gate's verdict, or the failed stage's artifacts.
// nil cause, or a cause naming neither, requires nothing.
func RemediationFailureEvidencePointers(cause *gate.RepassCause, pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	return remediationFailureEvidencePointers(cause, pointers)
}

// RequiredContextPointerNames lists a pointer set's distinct names, sorted, as
// the annotations and validation messages record them.
func RequiredContextPointerNames(pointers []apiv1.ContextPointer) []string {
	return requiredContextPointerNames(pointers)
}

// LearningEpisodeArtifactName / LearningEpisodePointerName are the #3874-gap
// injection names: the episode artifact a repass records, and the
// ContextPointer the re-entered stage receives it as.
//
// The POINTER name is load-bearing the same way `<gate>.diff` is:
// gate.readEpisodeHistory matches on the `learning.episode` prefix, so an
// engine that named the pointer anything else would inject an episode no
// reconciliation could ever read back — the correction would travel and be
// ignored, which is worse than not travelling.
func LearningEpisodeArtifactName(gateName string, sourceSeq uint64) string {
	return fmt.Sprintf("learning/episode-%s-%d.json", gateName, sourceSeq)
}

// LearningEpisodePointerName names the injected episode pointer.
func LearningEpisodePointerName(sourceSeq uint64) string {
	return fmt.Sprintf("learning.episode[%d]", sourceSeq)
}

// LearningEpisodeAppliesToRepass is the #3929 ruling, spelled once for both
// drivers: a learning episode is injected IF AND ONLY IF the branch is a true
// repass, and a branch is a true repass exactly when the gate result's repass
// attempt is at least 1.
//
// The caller — LearningEpisodeAppliesToBranch, which is what both drivers
// actually call — has already established the branch SHAPE: non-pass,
// non-escalated, and targeting a real stage rather than a reserved terminal.
// This is the SECOND question, and only this one: is the stage being
// re-entered, or entered for the first time?
//
// Note what is deliberately NOT among the caller's preconditions any more:
// `retryable`. #3929 was taken while both injection sites sat inside the
// retry-decision arm, so the ruling was read as applying only to failures the
// retry CLASSIFIER accepts. #3942 separated the two — a reviewer's
// needs-changes is a true repass that the classifier declines — and this
// predicate is unchanged by that, because the attempt is the attempt however
// the branch was classified.
//
// # Why the repass attempt, and not a re-derived "has the target completed?"
//
// repassAttempt is not a proxy for the answer, it IS the answer, already
// computed and already charged. internal/gate's trackRepass returns 0 outright
// unless IsReentry(target) holds — wired to visitedStages on both the
// sequential and the parallel walk — and otherwise returns the target's
// position in the repass budget it just charged. internal/engine's
// resolveGateOutcome does the same from `upstream[wfTarget(g, outcome)]`. Both
// then journal the number on the retry-decision annotation, where
// E2-retry-decision-annotation already asserts the two sides agree on it.
//
// So the predicate is EVIDENCED rather than re-derived, and that distinction
// is the point of sharing it. The engine previously asked the question a
// second way (gateSendsBack: is the target present in the upstream map?),
// which was equivalent by construction at its one call site and therefore
// silently able to stop being equivalent. Charging a repass budget is the act
// that means "this stage is being asked to do its work again"; a correction
// belongs exactly where that happened.
//
// # What a forward branch is instead
//
// A fail branch that routes ONWARD, to a stage that has not run, is a
// disposition: the park-*/release-* stages every lane uses to record, escalate
// or hand off. Such a stage has not done anything that could need correcting,
// and pr-remediation's park-infrastructure-failure says so in its own
// escalation text ("no implementation defect was established") while the
// injected episode would assert the opposite, under a content-addressed
// signature that gate.readEpisodeHistory correlates across runs. It would also
// downgrade a first-run stage's produced integrity to derived — admission
// control — for a correction it is not receiving.
func LearningEpisodeAppliesToRepass(repassAttempt int) bool {
	return repassAttempt >= 1
}

// LearningEpisodeBranch is the resolved gate branch the injection predicate
// reads: exactly the four fields of a gate verdict that decide whether the
// branch is a correctable re-entry, and nothing else.
//
// It is a struct rather than the driver's own result type because the two
// drivers hold that verdict in different types — internal/gate.Result on the
// local runner, internal/engine.gateResult on the Temporal engine — and
// internal/engine imports internal/runner, never the other way round.
type LearningEpisodeBranch struct {
	// Outcome is the gate's resolved outcome: "pass"/"fail"/"infra" for an
	// automated check, or the reviewer's verdict decision for an agentic gate.
	Outcome string
	// Escalated reports that the gate exhausted a budget or force-escalated,
	// in which case the branch is a disposition rather than a correction.
	Escalated bool
	// Target is the resolved branch target.
	Target string
	// Attempt is the repass attempt internal/gate.trackRepass (local runner)
	// or internal/engine.resolveGateOutcome charged to Target.
	Attempt int
}

// LearningEpisodeAppliesToBranch is the CANONICAL injection predicate, spelled
// once for both drivers: a learning episode is injected if and only if the
// resolved branch is a correctable re-entry — a non-pass, non-escalated branch
// whose target is a real stage rather than a reserved terminal, and which the
// gate has charged a repass attempt of at least 1.
//
// # Why this exists separately from the retry decision
//
// #3929 ruled that "true repass" is the whole predicate, and hoisted
// LearningEpisodeAppliesToRepass so both drivers would answer it the same way.
// What it left in place was the condition WRAPPING that call on both sides:
// the injection sat inside the retry-decision arm, so it also required
// `retryable` — internal/runner.retryFailureClassForGateResult, which is true
// only for an automated `status-equals` gate failing on
// `nonzero_exit`/`base_sync_conflict`, or for ANY gate resolving `infra`.
//
// That is a CLASSIFICATION question ("is this failure's outcome knowable
// without dispatching the checker, and is it policy or infrastructure?"), and
// it answers a different question from the one the injection asks ("is a stage
// being asked to do its work again, with something to learn from?"). An
// AGENTIC reviewer gate resolving `needs-changes` back into `implement` is the
// canonical true repass of the whole system, and the classifier declines it:
// the evaluator is not `automated`, the outcome is not `infra`, so `retryable`
// was false and the episode was never built. The main implementation lane's
// reviewer→implement loop — reference-workflows/.../implementation.yaml's
// `review` gate, and pr-remediation.yaml's — therefore never received the
// correction that internal/gate.reconcileLearningFindings exists to read back,
// while the deterministic `nonzero_exit` lanes did.
//
// # What it deliberately does NOT do
//
// It does not change ROUTING, and it does not change CLASSIFICATION. The
// retry-decision annotation, the repass budget, `routeRetryDecision`'s return
// and the failure class that annotation carries are all untouched: a branch
// the classifier declines still re-enters its stage through the ordinary
// advance path, and still carries no retry-decision annotation. Only the
// episode is widened, and only onto branches that were already re-entries.
//
// The reserved-terminal and non-pass/non-escalated guards are retained
// verbatim from internal/runner.routeRetryDecision so that an ONWARD or
// TERMINAL branch remains excluded: a stage that has not run has produced
// nothing to correct (#3929), and a disposition branch would have a
// content-addressed correction asserted against work it never did.
func LearningEpisodeAppliesToBranch(b LearningEpisodeBranch) bool {
	if b.Outcome == gate.OutcomePass || b.Escalated {
		return false
	}
	switch b.Target {
	case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
		return false
	}
	return LearningEpisodeAppliesToRepass(b.Attempt)
}

// LearningEpisodeBranchFor adapts the local runner's gate verdict to the
// canonical predicate's input.
func LearningEpisodeBranchFor(r gate.Result) LearningEpisodeBranch {
	return LearningEpisodeBranch{
		Outcome: r.Outcome, Escalated: r.Escalated, Target: r.Target, Attempt: r.Attempt,
	}
}

// LearningEpisodeInjectedKind is the runner.annotation kind recording an
// injection.
const LearningEpisodeInjectedKind = "learning.episode.injected"

// LearningEpisodeInput is everything BuildLearningEpisode needs that is not
// derivable from the verdict and the subject result. It is a struct rather
// than eight positional arguments because both callers fill it from different
// places (the local runner from StartInput and gate.Result, the engine from
// RunInput and its own gateResult) and a positional list would be silently
// mis-ordered.
type LearningEpisodeInput struct {
	RunID          string
	Workflow       string
	WorkflowDigest string
	GooberDigest   string
	Gate           string
	Stage          string
	// SourceSeq is the journal sequence of the event this episode corrects —
	// the reviewer's gate.evaluated, or the failed stage.finished.
	SourceSeq uint64
	// SourceAttempt is that event's attempt number; 0 falls back to 1.
	SourceAttempt int
	// TargetNextAttempt is the attempt number the stage being RE-ENTERED is
	// about to take, derived from the TARGET's own entry history rather than
	// from the subject's (#3931).
	//
	// The two are only the same number on a trivial send-back, where the fail
	// branch re-enters the stage that produced the subject. Every nontrivial
	// send-back in a shipped lane — implementation.yaml's
	// `local-gate: fail -> implement` over a `local-ci` subject,
	// `ci-gate: fail -> remediate-ci` over `ci-poll`, pr-remediation's
	// `finding-responses-gate: fail -> guard-before-implement` over
	// `validate-finding-responses` — has a subject that runs once per cycle
	// and a target several re-entries in, so subject+1 addressed the
	// correction to a re-entry of the target that either had not happened yet
	// or had already happened with different content.
	//
	// 0 means "the target has no entry history to read", which is the
	// forward-branch shape LearningEpisodeAppliesToRepass already refuses, and
	// the shape a bare unit fixture has. It falls back to SourceAttempt+1, the
	// pre-#3931 derivation, so the degenerate case is unchanged — and so a
	// Temporal history recorded before the change replays to its own bytes.
	TargetNextAttempt int
	// Verdict is the reviewer's verdict when the repass came from a reviewer
	// gate, nil for a stage-failure repass.
	Verdict *apiv1.Verdict
	// SourceResult is the subject stage's own result.
	SourceResult apiv1.ResultEnvelope
	// VerdictPointer is the "<gate>.verdict" pointer injected alongside, if
	// any: its artifact leads the episode's evidence list.
	VerdictPointer *apiv1.ContextPointer
}

// BuildLearningEpisode assembles the learning episode a repass injects into
// the stage it re-enters, identically on both runners.
//
// It is the whole of recordLearningInjection except the two side effects
// (record the artifact, append the annotation), which necessarily differ: the
// local runner writes to a journal.Run, the engine appends deterministic ops
// to its projection. Everything that ends up in the episode's BYTES — and
// therefore in its digest, and therefore on the conformance surface — is here.
func BuildLearningEpisode(in LearningEpisodeInput) learning.Episode {
	sourceAttempt := in.SourceAttempt
	if sourceAttempt == 0 {
		sourceAttempt = 1
	}
	// #3931: the two numbers mean two different things. SourceAttempt says
	// WHICH failure the episode is about — the subject's own attempt, which is
	// what makes the episode identify its cause. NextAttempt says WHO the
	// correction is addressed to — the target's next attempt, because the
	// episode's contract is "on attempt N of this stage, do this differently".
	nextAttempt := in.TargetNextAttempt
	if nextAttempt <= 0 {
		nextAttempt = sourceAttempt + 1
	}
	findings := learningFindingsForRepass(in.Gate, in.Stage, in.Verdict, in.SourceResult)
	evidence := learningEvidence(in.VerdictPointer, in.Verdict, in.SourceResult)
	classification := apiv1.LearningValidation
	if len(findings) > 0 {
		classification = findings[0].LearningClassification
	}
	correction := strings.TrimSpace(in.SourceResult.Summary)
	if in.SourceResult.Error != nil && strings.TrimSpace(in.SourceResult.Error.Message) != "" {
		correction = strings.TrimSpace(in.SourceResult.Error.Message)
	}
	if in.Verdict != nil {
		correction = strings.TrimSpace(in.Verdict.Rationale)
	}
	episode := learning.Episode{
		Schema:             learning.EpisodeSchema,
		SourceRunID:        in.RunID,
		SourceSeq:          in.SourceSeq,
		Workflow:           in.Workflow,
		Stage:              in.Stage,
		Gate:               in.Gate,
		SourceAttempt:      sourceAttempt,
		NextAttempt:        nextAttempt,
		WorkflowDigest:     in.WorkflowDigest,
		GooberDigest:       in.GooberDigest,
		EffectiveVersion:   learning.EffectiveVersion(in.WorkflowDigest, in.GooberDigest),
		Signature:          learning.CombinedSignature(findings),
		Classification:     classification,
		RecommendedAction:  learning.RecommendedAction(classification),
		CorrectionFeedback: correction,
		Findings:           findings,
		Actions:            learning.ActionsForFindings(findings),
		Evidence:           evidence,
		Outcome:            learning.OutcomeUnresolved,
	}
	episode.ID = learning.EpisodeID(episode)
	return episode
}

// LearningEpisodeAddressing is the attempt arithmetic an injected learning
// episode is addressed by, resolved from a run's journaled events.
//
// It is one value rather than three returns because the three are resolved by
// a single backwards scan and only make sense together: the source event, the
// attempt that produced it, and the attempt the repass target is about to
// take. Splitting them is what let #3931 happen — the builder had the subject's
// number and used it for both questions.
type LearningEpisodeAddressing struct {
	// SourceSeq is the journal sequence of the event the episode corrects.
	//
	// It is not decorative: it names the episode artifact
	// (LearningEpisodeArtifactName) and its context pointer
	// (LearningEpisodePointerName), so it is part of the conformance surface.
	SourceSeq uint64
	// SourceAttempt is that event's own attempt number. 0 means the scan found
	// no source event; BuildLearningEpisode falls back to 1.
	SourceAttempt int
	// TargetNextAttempt is the attempt number the stage being RE-ENTERED is
	// about to take: the number of times it has already been entered in this
	// branch, plus one. 0 means the target has never been entered, which is
	// the forward-branch shape LearningEpisodeAppliesToRepass refuses.
	TargetNextAttempt int
}

// ResolveLearningEpisodeAddressing resolves which journaled event an injected
// learning episode is ABOUT, which attempt produced it, and which attempt of
// the TARGET the correction is addressed to.
//
// Exported for #3882 so the engine, whose events live in workflow state rather
// than on disk, resolves the identical addressing rather than inventing its
// own numbering — and, since #3931, so that the target-side arithmetic cannot
// be derived twice either.
//
// The target scan is scoped to the SOURCE event's branch. A repass happens in
// the branch the failure happened in, and two sibling parallel branches may
// walk stages of the same name at different attempt counts; counting across
// them would address the correction to an attempt of somebody else's stage.
func ResolveLearningEpisodeAddressing(
	events []journal.Event, gateName, sourceStage, target string, reviewer bool,
) LearningEpisodeAddressing {
	out := LearningEpisodeAddressing{}
	branch := 0
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if reviewer && event.Type == journal.EventGateEvaluated && event.Gate == gateName {
			attempt, _ := runnerInt(event.Runner["repassAttempt"])
			out.SourceSeq, out.SourceAttempt, branch = event.Seq, attempt, event.Branch
			break
		}
		if !reviewer && event.Type == journal.EventStageFinished && event.Stage == sourceStage {
			out.SourceSeq, out.SourceAttempt, branch = event.Seq, event.Attempt, event.Branch
			break
		}
	}
	out.TargetNextAttempt = learningTargetNextAttempt(events, target, branch)
	return out
}

// learningTargetNextAttempt is the target half of the addressing: the number
// of times the target stage has already been ENTERED in branch, plus one.
//
// Entries, not stage.Attempt values. journal.Event.Attempt on a stage.* event
// is the attempt number WITHIN one entry — the Task.Retry policy loop — and a
// gate sending a stage back starts a fresh entry at attempt 1 (stepTask seeds
// startAttempt = 1 on every repass). So the target's own stage.finished
// attempts are 1, 1, 1 no matter how many times it has been repassed, and the
// number a correction has to be addressed by is which RE-ENTRY it feeds. That
// is the same counter internal/gate's trackRepass charges to
// RepassAttempts[target], derived here from the target's own history rather
// than taken from the gate result, so it stays correct for a target re-entered
// by something other than this gate.
//
// An entry is a stage.started whose attempt does not continue the previous
// one: attempts 1,2,3 are one entry with two policy retries, and a following
// attempt 1 opens the second. A resumed entry (startAttempt = interrupted + 1)
// continues rather than opens, and its attempt-1 start is already in the
// journal from before the interruption.
//
// Only stage.started counts. The runner.annotation events an injection itself
// writes are attributed to the target at the attempt they FEED, so counting
// them would make each injection advance the number the next one reads — the
// episode addressing its own successor rather than the dispatch it feeds.
func learningTargetNextAttempt(events []journal.Event, target string, branch int) int {
	if target == "" {
		return 0
	}
	entries, previous := 0, 0
	for _, event := range events {
		if event.Type != journal.EventStageStarted || event.Branch != branch || event.Stage != target {
			continue
		}
		attempt := event.Attempt
		if attempt == 0 {
			attempt = 1
		}
		if attempt <= previous || previous == 0 {
			entries++
		}
		previous = attempt
	}
	if entries == 0 {
		return 0
	}
	return entries + 1
}

// LearningEpisodeAnnotation is the runner.annotation payload recorded beside
// an injected episode, spelled once so both runners' operator surfaces read
// the same keys.
//
// sourceStage is here because of #3931. The annotation is attributed to the
// TARGET — it is the correction the target's next entry will be dispatched
// with — so the event's own Stage and Attempt now name the target on a
// nontrivial send-back rather than the failing stage. Without sourceStage the
// payload would say which failure the episode is about only indirectly,
// through the signature, and `goobers trace` would have to resolve the
// artifact to answer "what failed". sourceSeq addresses the corrected EVENT;
// sourceStage names it.
func LearningEpisodeAnnotation(episode learning.Episode, target, artifactPath, artifactDigest string) map[string]any {
	identities := make([]string, 0, len(episode.Findings))
	for _, finding := range episode.Findings {
		identities = append(identities, finding.ID)
	}
	return map[string]any{
		"kind":               LearningEpisodeInjectedKind,
		"episodeId":          episode.ID,
		"sourceRunId":        episode.SourceRunID,
		"sourceSeq":          episode.SourceSeq,
		"gate":               episode.Gate,
		"target":             target,
		"sourceStage":        episode.Stage,
		"sourceAttempt":      episode.SourceAttempt,
		"nextAttempt":        episode.NextAttempt,
		"signature":          episode.Signature,
		"classification":     episode.Classification,
		"recommendedAction":  episode.RecommendedAction,
		"findingIdentities":  identities,
		"correctionFeedback": episode.CorrectionFeedback,
		"episodePath":        artifactPath,
		"episodeDigest":      artifactDigest,
	}
}
