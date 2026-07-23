package gate

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// Journal is what Evaluator needs to record a gate verdict. It is satisfied
// directly by *internal/journal.Run (issue #8) — declared locally so this
// package's tests can use a fake instead of standing up a real run journal.
type Journal interface {
	Append(ev journal.Event) error
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// recordStart durably marks a gate evaluation before its evaluator is
// dispatched. repassAttempt is the prospective consecutive non-pass count:
// recordVerdict replaces it with the actual post-evaluation count, while a
// dangling marker lets Resume charge an interrupted evaluation to the budget.
func recordStart(j Journal, gateName string, repassAttempt int) error {
	if j == nil {
		return nil
	}
	return j.Append(journal.Event{
		Type:   journal.EventGateStarted,
		Gate:   gateName,
		Runner: map[string]any{"repassAttempt": repassAttempt},
	})
}

// recordEvaluatorRetry journals one failed, retryable gate-evaluator attempt
// (#765): a transient evaluator error the Evaluator will retry within the gate's
// declared RetryPolicy bound. Before #765 a transient reviewer-harness failure
// left no gate journal record at all — the reviewer error short-circuited out
// of Evaluate before recordVerdict, leaving only a dangling gate.started marker.
// It mirrors the runner's per-attempt stage-retry journaling: a generic
// EventError annotated (Runner namespace) with the 1-based attempt number and an
// "infra" retry class, so the retried attempt is visible in `goobers trace`.
func recordEvaluatorRetry(j Journal, gateName string, attempt int, err error) error {
	if j == nil {
		return nil
	}
	return j.Append(journal.Event{
		Type:  journal.EventError,
		Gate:  gateName,
		Error: &journal.ErrorDetail{Code: "evaluator_transient", Message: err.Error()},
		Runner: map[string]any{
			"evaluatorAttempt":  attempt,
			"retryFailureClass": "infra",
		},
	})
}

// recordVerdict journals one gate evaluation as a gate.evaluated event: Gate,
// Verdict (the outcome string), Target, and Escalated are the flat,
// conformance-normative fields §4 relies on. The repass attempt count and a
// compatibility copy of the escalation marker remain runner-local annotations
// (the Runner namespace — always excluded from conformance, ARCHITECTURE.md
// §4/§3.3). For agentic gates the full Verdict (decision, rationale, evidence,
// findings) is recorded as an artifact so its detail survives for the Tutor
// without bloating the flat event stream, and the event's Ref points at it.
//
// duplicateDiff (issue #316) is likewise a Runner-namespace annotation. The
// digest itself is only journaled when non-empty, mirroring repassAttempt's
// seeding contract: internal/runner/resume.go's gateDiffSeed reconstructs
// Evaluator.LastDiffDigest from each gate's last such event on resume, the
// same way gateRepassSeed reconstructs Attempts.
//
// verdictCacheHit (issue #523) is a third Runner-namespace annotation,
// alongside duplicateDiff: true when this attempt reused
// Evaluator.CachedVerdict instead of invoking the reviewer. Unlike
// duplicateDiff/repassAttempt it has no seeding contract to preserve on
// resume — a cache hit is a one-shot fact about how THIS attempt's verdict
// was obtained, not run-scoped counter state. The reused Verdict's own
// SourceRunID (unchanged by the reuse) is what makes a cache-hit event
// auditable in `goobers trace`: the annotation says "this run skipped the
// reviewer," the artifact says which run actually ran it.
//
// recordVerdict returns the verdict's journaled ArtifactPointer (nil when
// r.Verdict is nil, or when j is nil) so the caller (Evaluate) can attach it
// to Result.VerdictArtifact — the same artifact this function just recorded,
// handed back rather than recomputed, for the runner to surface as a repass
// ContextPointer (issue #412).
func recordVerdict(j Journal, r Result, diffDigest string) (*apiv1.ArtifactPointer, error) {
	if j == nil {
		return nil, nil
	}
	runner := map[string]any{
		"repassAttempt":   r.Attempt,
		"escalated":       r.Escalated,
		"duplicateDiff":   r.DuplicateDiff,
		"verdictCacheHit": r.CacheHit,
	}
	if r.Interrupted {
		runner["interrupted"] = true
	}
	if diffDigest != "" {
		runner["diffDigest"] = diffDigest
	}
	ev := journal.Event{
		Type:      journal.EventGateEvaluated,
		Gate:      r.Gate,
		Verdict:   r.Outcome,
		Target:    r.Target,
		Escalated: r.Escalated,
		Runner:    runner,
	}
	var artifact *apiv1.ArtifactPointer
	if r.Verdict != nil {
		data, err := json.Marshal(r.Verdict)
		if err != nil {
			return nil, fmt.Errorf("gate: marshal verdict for journal: %w", err)
		}
		name := fmt.Sprintf("verdict/%s-%d.json", r.Gate, r.Attempt)
		ref, err := j.RecordArtifact(name, data)
		if err != nil {
			return nil, fmt.Errorf("gate: record verdict artifact: %w", err)
		}
		ev.Name = name
		ev.Ref = &ref
		artifact = &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "application/json"}
	}
	if err := j.Append(ev); err != nil {
		return nil, err
	}
	return artifact, nil
}
