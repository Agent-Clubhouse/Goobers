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

// recordVerdict journals one gate evaluation as a gate.evaluated event: Gate,
// Verdict (the outcome string), and Target are the flat, conformance-normative
// fields §4 relies on; the repass attempt count and whether the budget forced
// an escalation are runner-local annotations (the Runner namespace — always
// excluded from conformance, ARCHITECTURE.md §4/§3.3). For agentic gates the
// full Verdict (decision, rationale, evidence, findings) is recorded as an
// artifact so its detail survives for the Tutor without bloating the flat
// event stream, and the event's Ref points at it.
//
// duplicateDiff (issue #316) is likewise a Runner-namespace annotation. The
// digest itself is only journaled when non-empty, mirroring repassAttempt's
// seeding contract: internal/runner/resume.go's gateDiffSeed reconstructs
// Evaluator.LastDiffDigest from each gate's last such event on resume, the
// same way gateRepassSeed reconstructs Attempts.
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
	runner := map[string]any{"repassAttempt": r.Attempt, "escalated": r.Escalated, "duplicateDiff": r.DuplicateDiff}
	if diffDigest != "" {
		runner["diffDigest"] = diffDigest
	}
	ev := journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    r.Gate,
		Verdict: r.Outcome,
		Target:  r.Target,
		Runner:  runner,
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
