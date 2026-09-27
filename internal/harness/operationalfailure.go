package harness

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// operationalFailureCodeMarkers are the self-reported error codes that name an
// OPERATIONAL failure of the environment the stage ran in — a restore/install
// step that did not complete — as opposed to a per-item business or content
// block.
//
// Matched on the normalized code alone, by EXACT equality rather than the
// substring rule isMissingCapabilityCode uses. #5262 requires that an unknown
// or ambiguous block is never silently converted, and substring matching cannot
// promise that: a hypothetical `OPTIONAL_DEPENDENCY_RESTORE_FAILED_BUT_CONTINUED`
// would match a `DEPENDENCY_RESTORE_FAILED` substring while meaning the
// opposite. Exact equality makes the recognized set exactly as wide as the
// evidence for it.
//
// Deliberately narrow. This set grows only when operational evidence
// establishes the classification for a specific code — not by analogy to a code
// that looks similar. DEPENDENCY_NOT_MET in particular is NOT here and must not
// be added: an unmet dependency is the genuine content/dependency block that
// #544 settled should escalate, and #2197 already recorded the decision to
// leave it untouched.
var operationalFailureCodeMarkers = map[string]bool{
	"DEPENDENCY_RESTORE_FAILED":   true,
	"DEPENDENCIES_RESTORE_FAILED": true,
}

// isOperationalFailureCode reports whether code names a recognized operational
// environment failure. Normalization mirrors isMissingCapabilityCode's so the
// two matchers cannot disagree about what a producer's separator style meant.
func isOperationalFailureCode(code string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	normalized = strings.NewReplacer("-", "_", " ", "_", ".", "_", ":", "_").Replace(normalized)
	return operationalFailureCodeMarkers[normalized]
}

// reclassifyOperationalFailureBlock is #5262: an operational failure of the
// environment, reported by the producer as `blocked`, is reclassified to
// `failure` so it reaches the remediation gate a workflow declared after the
// stage instead of terminating the run first.
//
// Why the status has to change rather than the routing: blocked is terminal by
// the #544 ruling — it ends the run at PhaseEscalated, which needs-human-parks
// every item the run claimed. That is correct for a genuine content or
// dependency block and wrong for a dependency restore that failed, which is
// exactly the kind of fault a remediation gate exists to repair or retry. But
// blanket blocked-to-next routing would be incompatible with #544 and unsafe,
// so this narrows by CODE instead: only a code whose meaning is established as
// operational is converted, and every other block keeps the #544 behavior.
//
// Three properties #5262 requires, all deliberate:
//
//  1. The original error is PRESERVED — code and message both, untouched. The
//     gate evaluator binds error.code and error.retryable as gate inputs
//     (internal/gate.InputKeyErrorCode / InputKeyErrorRetryable), so rewriting
//     the code to a synthetic HARNESS_* value would be the one change that
//     stops a gate authored against the real producer code from branching on
//     it. The reclassification records itself in Outputs and Summary instead.
//
//  2. Retryable becomes true. An operational failure is the case where
//     repeating the work can legitimately succeed, and it is how the gate is
//     told that "infrastructure retry" is among its options rather than
//     inferring it from the code. This does not cause an automatic retry:
//     the runner's retry accounting keys off a dispatch-level
//     invoke.InfrastructureFailure, not a result envelope's Retryable field.
//
//  3. Nothing is escalated on sight. A recognized operational code is not in
//     the runner's escalateErrorCodes set and is retryable, so
//     isNonRetryableEscalation is false and the result follows the ordinary
//     failure route — into the declared Next gate, else PhaseFailed. A
//     workflow with no gate after the stage is therefore no worse off than
//     before: it ends failed rather than escalated, which is the honest
//     status for an environment fault and still does not park the items.
//
// Runs on the shared harness result path, so the local runner and the Temporal
// engine observe the identical reclassified envelope and cannot diverge.
func reclassifyOperationalFailureBlock(result *apiv1.ResultEnvelope) {
	if result == nil || result.Status != apiv1.ResultBlocked || result.Error == nil {
		return
	}
	if !isOperationalFailureCode(result.Error.Code) {
		return
	}
	result.Status = apiv1.ResultFailure
	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	// Names the reclassification for diagnostics and for a gate that would
	// rather branch on the fact than on the code set. The original code stays
	// readable in result.Error.Code, so no evidence is lost by recording this.
	result.Outputs["operationalFailure"] = true
	result.Outputs["reclassifiedFromBlocked"] = true
	result.Error.Retryable = true
	// authoredCause folds the producer's own code, message and summary into one
	// line (and yields "(no detail)" rather than an empty string when the
	// producer supplied none), so replacing Summary here cannot lose the
	// narrative the agent wrote — #5262 requires the diagnostic detail be
	// retained, not just the status corrected.
	result.Summary = "operational environment failure (reported by the agent as blocked): " + authoredCause(*result)
}
