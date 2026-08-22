package gate

import (
	"crypto/sha256"
	"encoding/hex"
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
// dispatched. repassAttempt is the prospective per-gate count, allowing Resume
// to recover a dangling evaluation before its branch target is known.
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

func recordVerdictValidationRetry(j Journal, gateName string, attempt int, err error) error {
	if j == nil {
		return nil
	}
	return j.Append(journal.Event{
		Type:  journal.EventError,
		Gate:  gateName,
		Error: &journal.ErrorDetail{Code: "verdict_invalid", Message: err.Error()},
		Runner: map[string]any{
			"evaluatorAttempt":  attempt,
			"retryFailureClass": "policy",
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
// digest itself is only journaled when non-empty. internal/runner/resume.go
// reconstructs the gate, target-stage, and digest state from these annotations.
// reason (issue #3250) mirrors it: the machine-readable ReasonUnchangedRepass
// code, journaled only when non-empty, so a reader can match on a stable
// string instead of re-deriving "was this an unchanged repass" from
// duplicateDiff/repassCause alone.
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
		"gateAttempt":     r.GateAttempt,
		"escalated":       r.Escalated,
		"duplicateDiff":   r.DuplicateDiff,
		"verdictCacheHit": r.CacheHit,
	}
	if r.RepassTarget != "" {
		runner["repassTarget"] = r.RepassTarget
	}
	if r.Interrupted {
		runner["interrupted"] = true
	}
	if diffDigest != "" {
		runner["diffDigest"] = diffDigest
	}
	if r.RepassCause != nil {
		runner["repassCause"] = r.RepassCause
	}
	if r.Reason != "" {
		runner["reason"] = r.Reason
	}
	if r.Verdict != nil && len(r.Verdict.Findings) > 0 {
		identities := make([]string, 0, len(r.Verdict.Findings))
		for _, finding := range r.Verdict.Findings {
			data, err := json.Marshal(struct {
				Message  string `json:"message"`
				Location string `json:"location,omitempty"`
				Class    string `json:"class,omitempty"`
			}{finding.Message, finding.Location, string(finding.Class)})
			if err != nil {
				return nil, fmt.Errorf("gate: encode finding identity: %w", err)
			}
			sum := sha256.Sum256(data)
			identities = append(identities, "sha256:"+hex.EncodeToString(sum[:]))
		}
		runner["findingIdentities"] = identities
	}
	ev := journal.Event{
		Type:      journal.EventGateEvaluated,
		Actor:     r.Actor,
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
		ev.Integrity = ref.Integrity
		artifact = &apiv1.ArtifactPointer{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
			MediaType: "application/json", Integrity: ref.Integrity,
		}
	}
	if err := j.Append(ev); err != nil {
		return nil, err
	}
	return artifact, nil
}
