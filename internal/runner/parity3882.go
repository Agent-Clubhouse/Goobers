package runner

import (
	"encoding/json"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/learning"
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
		NextAttempt:        sourceAttempt + 1,
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

// LearningEpisodeAnnotation is the runner.annotation payload recorded beside
// an injected episode, spelled once so both runners' operator surfaces read
// the same keys.
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
