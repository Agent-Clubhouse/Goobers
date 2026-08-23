package goobernetes

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// RepassObserver is S5's named observer (goobernetes-smoke.md §4 S5): "the
// repass attempt's journal — attemptClass policy..., runner.* node differing
// from the first attempt's, and the bound <gate>.verdict pointer in its
// recorded inputs."
const RepassObserver = "repass StageAttempt.Class == \"policy\" + Placement.Node vs. the original attempt's + a <gate>.verdict apiv1.ContextPointer in recorded inputs (internal/runner/run.go g.Name+\".verdict\" naming)"

// GateVerdictPointerName is the `<gate>.verdict` contextPointer naming
// convention S5 checks for, exactly as internal/runner builds it (e.g.
// internal/runner/run.go:107, :1784, :2063: `g.Name + ".verdict"`).
func GateVerdictPointerName(gate string) string { return gate + ".verdict" }

// AssertRepassFreshNodeWithVerdict is S5: a gate fails a stage, the repass
// attempt runs in a fresh pod on a DIFFERENT node, and receives the gate's
// just-recorded Verdict as the `<gate>.verdict` contextPointer plus prior
// commits.
//
// original and repass are the two StageAttempt rows from the same stage's
// AttemptList (readservice.AttemptList.Attempts — internal/readservice's
// real per-stage attempt shape). recordedInputs is the repass attempt's
// bound context pointers; there is no read-model field exposing "recorded
// inputs" today (they are runner-internal state, internal/runner/parallel_run.go
// `pointers []apiv1.ContextPointer`, never journaled verbatim under a
// queryable name), so a live driver supplies whatever it captured — the
// stage's raw journal payload or a future read-model extension. That gap is
// itself named in the smoke doc: "This is the single least-proven flow in
// the system."
func AssertRepassFreshNodeWithVerdict(gate string, original, repass readservice.StageAttempt, recordedInputs []apiv1.ContextPointer) AssertionResult {
	if gate == "" {
		return invalid("no gate named", nil)
	}
	if repass.Class != string(journal.AttemptPolicy) {
		return classify("", false,
			fmt.Sprintf("repass attempt's Class is %q, want %q (journal.AttemptPolicy) — a repass is work, not weather", repass.Class, journal.AttemptPolicy),
			nil, repass)
	}
	if original.Placement == nil || original.Placement.Node == "" || repass.Placement == nil || repass.Placement.Node == "" {
		return invalid("original or repass attempt carries no Placement.Node", map[string]any{"original": original, "repass": repass})
	}
	if original.Placement.Node == repass.Placement.Node {
		return classify("", false,
			fmt.Sprintf("repass ran on the SAME node %q as the original attempt — S5 requires a fresh node", original.Placement.Node),
			nil, map[string]any{"original": original, "repass": repass})
	}
	if repass.Placement.Pod == "" || (original.Placement.Pod != "" && repass.Placement.Pod == original.Placement.Pod) {
		return classify("", false, "repass did not execute in a fresh pod (S1 subsumed: repass reused the original attempt's pod)", nil, map[string]any{"original": original, "repass": repass})
	}

	wantName := GateVerdictPointerName(gate)
	found := false
	for _, p := range recordedInputs {
		if p.Name == wantName {
			found = true
			break
		}
	}
	if !found {
		return classify("", false,
			fmt.Sprintf("repass attempt has no %q contextPointer in its recorded inputs — it 'worked' by losing the gate's context, per S5's explicit fail condition", wantName),
			nil, recordedInputs)
	}

	return classify("", true, "", map[string]any{"original": original, "repass": repass, "verdictPointer": wantName}, nil)
}
