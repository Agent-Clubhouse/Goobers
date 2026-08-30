package gate

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This file is the SHARED half of decision 005's implementation-lane parity
// bundle (#3882): the pieces of gate policy the Temporal engine's walk needs
// to reach, exported once here rather than re-declared there.
//
// The rule it exists to enforce is the #624 shared-constant pattern. Every
// value below is something a reviewer verdict is SYNTHESIZED from, or a
// finding-lifecycle transition a verdict is rewritten by, and both are policy
// the two runners must never disagree about: a repass that the local runner
// short-circuits with "no change in response" and the engine sends back to a
// real reviewer is not a substrate difference, it is a different product. So
// the local paths below (Evaluate's own arms) call exactly these functions,
// and a change to the text or the rule reaches both runners or neither.

// ArtifactBytes resolves one artifact pointer's bytes.
//
// It is the seam that makes the finding-lifecycle rules substrate-neutral. The
// local runner resolves a pointer against the run directory on disk
// (apiv1.ArtifactPointer.Resolve); the engine has no run directory while it is
// walking — its journal is deterministic workflow state, and an episode's
// bytes live in the projection's own artifact ops — so it resolves against
// that instead. Both hand the same rule the same bytes.
//
// A nil resolver means "no evidence is reachable", which every rule below
// treats exactly as the local runner treats an empty journal root: no history,
// no suppression, no disproval. Fail-open is correct here and only here — the
// rules can only ever REMOVE findings a reviewer raised, so an unreachable
// history can lose a suppression, never invent one.
type ArtifactBytes func(apiv1.ArtifactPointer) ([]byte, error)

// FindingResolution is the exported spelling of the finding-lifecycle
// bookkeeping one verdict reconciliation produced: which finding identities
// this episode resolved, suppressed, reopened, or disproved.
//
// findingResolution stays as an alias below rather than a second struct, so
// the value Evaluate threads into Result and the value an external caller
// reads are the same type and cannot drift.
type FindingResolution struct {
	Resolved          []string
	Suppressed        []string
	Reopened          []string
	Disproven         []string
	DisprovenFindings []apiv1.Finding
	// AllSuppressed reports that every finding on a needs-changes verdict was
	// a suppressed re-raise, so the verdict was converted to pass.
	AllSuppressed bool
}

// findingResolution is the in-package spelling of FindingResolution. An alias,
// not a definition, for the reason DispatchStageResult's alias exists in the
// engine: exporting a type must never introduce a second one.
type findingResolution = FindingResolution

// EmptyDiffVerdict is the verdict Evaluate synthesizes for #415's empty-diff
// fast-fail: the agentic subject stage committed no change at all, so there is
// nothing for a reviewer to evaluate and nothing for a repass to iterate on.
//
// Exported because the engine reaches the same conclusion from the same
// evidence (its own reviewer-diff capture came back empty) and must produce
// the byte-identical verdict — the verdict artifact's digest is normative on
// the parity surface, so a reworded copy on one side is a journal divergence.
func EmptyDiffVerdict() apiv1.Verdict {
	return apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: "runner: the implement stage produced no committed changes — failing without review, since an empty diff offers nothing to evaluate and a repass can only reproduce it",
	}
}

// DuplicateDiffVerdict is the verdict Evaluate synthesizes for #316's
// identical-diff guard: this repass produced a diff byte-identical to the one
// the reviewer already judged, so a second reviewer call can only repeat its
// prior verdict. cause is the repass cause when the caller resolved one, and
// nil falls back to naming the digest.
func DuplicateDiffVerdict(diffDigest string, cause *RepassCause) apiv1.Verdict {
	rationale := fmt.Sprintf("runner: this repass produced no change (digest %s)", diffDigest)
	if cause != nil {
		rationale = cause.String() + "; the implementer produced no change in response"
	}
	return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Rationale: rationale}
}

// ReconcileLearningFindings applies the learning-episode finding lifecycle to
// a reviewer verdict: carry prior episodes' identities and classifications
// onto the findings this episode raised, suppress a resolved identity re-raised
// without new evidence, reopen one that carries genuinely new evidence, and
// convert a needs-changes verdict whose findings were ALL suppressed into a
// pass.
//
// Exported for the engine's gate arm (#3882 item 4), which reaches it with a
// projection-backed resolver rather than a run directory. resolve may be nil.
func ReconcileLearningFindings(
	verdict apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	resolve ArtifactBytes,
	gateName, diffDigest string,
) (apiv1.Verdict, FindingResolution) {
	return reconcileLearningFindings(verdict, pointers, resolve, gateName, diffDigest)
}

// DisproveReviewerFindings removes findings that the reviewer's OWN diff
// evidence deterministically disproves, and converts the verdict to pass when
// every finding falls. Exported alongside ReconcileLearningFindings and for
// the same reason. resolve may be nil.
func DisproveReviewerFindings(
	verdict apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	resolve ArtifactBytes,
	gateName string,
) (apiv1.Verdict, bool) {
	return disproveReviewerFindings(verdict, pointers, resolve, gateName)
}

// FindingIDs lists the non-empty identities of a finding set, in order.
// Exported for the engine's own before/after disproval bookkeeping, which
// mirrors Evaluate's.
func FindingIDs(findings []apiv1.Finding) []string { return findingIDs(findings) }

// RemovedFindingIDs / RemovedFindings report what a rewrite dropped, given the
// identities (or findings) it started from and the set it kept.
func RemovedFindingIDs(before []string, remaining []apiv1.Finding) []string {
	return removedFindingIDs(before, remaining)
}

// RemovedFindings is RemovedFindingIDs' finding-level twin: the complete
// contract of every dropped finding, which is what a false-finding projection
// needs and an identity list cannot carry.
func RemovedFindings(before, remaining []apiv1.Finding) []apiv1.Finding {
	return removedFindings(before, remaining)
}

// ArtifactBytesFromRoot adapts a run directory to the ArtifactBytes seam — the
// local runner's resolver, and the one the file-backed callers in this package
// use. A root of "" yields nil, which is the documented "no evidence
// reachable" resolver.
func ArtifactBytesFromRoot(root string) ArtifactBytes {
	if root == "" {
		return nil
	}
	return func(a apiv1.ArtifactPointer) ([]byte, error) { return a.Resolve(root) }
}

// LearningFindingRecords renders a verdict's findings as the journal
// annotation shape gate.evaluated carries — the machine-readable finding
// ledger a later repass, a resume, and the learning store all read back.
//
// Exported for #3882: the engine journals its own gate.evaluated events
// (internal/engine/journal.go) rather than going through recordVerdict, so
// without this the two lanes would render the same findings differently and
// every downstream reader would have to know which lane wrote the run.
func LearningFindingRecords(findings []apiv1.Finding) []map[string]any {
	return learningFindingRecords(findings)
}
