package gate

import (
	"encoding/json"
	"fmt"

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
func recordVerdict(j Journal, r Result) error {
	if j == nil {
		return nil
	}
	ev := journal.Event{
		Type:    journal.EventGateEvaluated,
		Gate:    r.Gate,
		Verdict: r.Outcome,
		Target:  r.Target,
		Runner:  map[string]any{"repassAttempt": r.Attempt, "escalated": r.Escalated},
	}
	if r.Verdict != nil {
		data, err := json.Marshal(r.Verdict)
		if err != nil {
			return fmt.Errorf("gate: marshal verdict for journal: %w", err)
		}
		name := fmt.Sprintf("verdict/%s-%d.json", r.Gate, r.Attempt)
		ref, err := j.RecordArtifact(name, data)
		if err != nil {
			return fmt.Errorf("gate: record verdict artifact: %w", err)
		}
		ev.Name = name
		ev.Ref = &ref
	}
	return j.Append(ev)
}
