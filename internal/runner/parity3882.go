package runner

import (
	"fmt"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/runsemantics"
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

type baseSyncConflictArtifact = runsemantics.BaseSyncConflictDetail

type unpushedDiffMetadata = runsemantics.UnpushedDiffMetadata

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

// LearningEpisodeBranchFor adapts the local runner's gate verdict to the
// canonical predicate's input.
func LearningEpisodeBranchFor(r gate.Result) runsemantics.LearningEpisodeBranch {
	return runsemantics.LearningEpisodeBranch{
		Outcome: r.Outcome, Escalated: r.Escalated, Target: r.Target, Attempt: r.Attempt,
	}
}
