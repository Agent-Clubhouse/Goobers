package engine

// Stage-qualified inputsFrom (#562) on the engine walk — plan item E2 (#3874).
//
// The parity row (parity_row_inputsfrom_stage_qualified_test.go) proves the two
// runners agree. These tests pin the things a same-fixture diff cannot see:
//
//   - the DSL GATE, which is invisible to parity because both runners consult
//     the same workflow.SupportsStageQualifiedInputs — a fixture on a legacy
//     version would have both sides equally wrong and the row would stay green;
//   - the INTEGRITY grade a qualified reference carries, which is a property of
//     one binding rather than of a divergence;
//   - the tolerated-failure fall-through, whose whole point is that the map and
//     the walk's `upstream` disagree.

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// recordStage is the walk's own record path, spelled for a test that only cares
// about the outputs and the grade. It goes through recordCompletedStage rather
// than reaching into the shared map directly so these tests exercise the seam
// the walk actually uses.
func recordStage(completed completedStages, stage string, outputs map[string]interface{}, grade apiv1.Integrity) {
	recordCompletedStage(completed, apiv1.Task{Name: stage}, apiv1.ResultEnvelope{
		Status:    apiv1.ResultSuccess,
		Outputs:   outputs,
		Integrity: grade,
	})
}

// Under a DSL version that predates #562, a dotted value is ALWAYS a bare key —
// even when its prefix names a stage that ran and emitted that very key.
//
// This is the test the feature is most likely to lose. Adding qualified
// resolution to a shared resolver changes what an already-released version
// means underneath workflows authored against it, which is exactly the silent
// semantic drift the DSL version lifecycle exists to prevent (§3.1: a field's
// meaning may only change across a MAJOR bump). The gate is cheap to delete by
// accident — `qualified` is one bool threaded through four call sites — and
// nothing else in this package would notice.
func TestStageQualifiedInputsAreDSLGated(t *testing.T) {
	// The collision the gate exists for: an output literally NAMED
	// "pr-select.selectedNumber", and a stage named "pr-select" that emitted
	// "selectedNumber". A legacy definition must bind the literal.
	upstream := apiv1.ResultEnvelope{Outputs: map[string]interface{}{"pr-select.selectedNumber": "literal"}}
	completed := newCompletedStages()
	recordStage(completed, "pr-select", map[string]interface{}{"selectedNumber": 42}, apiv1.IntegrityTrusted)

	got, ok := resolveInputsFrom("pr-select.selectedNumber", upstream, completed, false)
	if !ok {
		t.Fatal("the literal bare key must still resolve when qualified resolution is off")
	}
	if got != "literal" {
		t.Errorf("value = %#v, want %q — a pre-#562 DSL version must not gain qualified resolution", got, "literal")
	}

	got, ok = resolveInputsFrom("pr-select.selectedNumber", upstream, completed, true)
	if !ok || got != 42 {
		t.Errorf("value = %#v (ok=%t), want 42 — the qualified reference must win once the feature is enabled", got, ok)
	}
}

// A qualified reference to a stage that did NOT run falls through to bare-key
// resolution. This is what keeps a legacy dotted output key working on a
// version that DOES support the feature — the whole backward-compatibility
// argument, and the reason the branch order is "stage exists?" before "split on
// dot", not the other way round.
func TestQualifiedReferenceFallsThroughToBareKey(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]interface{}{"metrics.p99": "legacy"}}
	completed := newCompletedStages()
	recordStage(completed, "build", map[string]interface{}{"sha": "abc"}, apiv1.IntegrityTrusted)

	got, ok := resolveInputsFrom("metrics.p99", upstream, completed, true)
	if !ok || got != "legacy" {
		t.Errorf("value = %#v (ok=%t), want %q — %q is not a stage that ran, so the whole string is a bare key",
			got, ok, "legacy", "metrics")
	}
}

// A qualified reference grades the PRODUCING stage, not the adjacent one.
//
// Integrity is the admission control on this path: runTask refuses to dispatch
// a stage whose resolved inputs sit below its declared minimumIntegrity. If the
// grade described the wrong stage the check would still RUN, and still pass or
// fail — just about a different producer. That is a silent authorization bug,
// not a visible one, so it is pinned directly.
func TestQualifiedInputsGradeTheProducingStage(t *testing.T) {
	completed := newCompletedStages()
	recordStage(completed, "scrape", map[string]interface{}{"body": "<html>"}, apiv1.IntegrityUnapproved)
	recordStage(completed, "verify", map[string]interface{}{"ok": true}, apiv1.IntegrityTrusted)
	// The immediately preceding stage is the TRUSTED one; a naive port would
	// grade every input by it.
	upstream := apiv1.ResultEnvelope{Integrity: apiv1.IntegrityTrusted, Outputs: map[string]interface{}{"ok": true}}

	if got := inputsFromIntegrity("scrape.body", upstream, completed, true); got != apiv1.IntegrityUnapproved {
		t.Errorf("integrity of scrape.body = %q, want %q — a qualified reference is graded by the stage it names",
			got, apiv1.IntegrityUnapproved)
	}
	if got := inputsFromIntegrity("ok", upstream, completed, true); got != apiv1.IntegrityTrusted {
		t.Errorf("integrity of the bare key = %q, want %q — a bare key is graded by the immediate upstream",
			got, apiv1.IntegrityTrusted)
	}
	// With the gate off, "scrape.body" is a bare key and must be graded as one
	// — grading it by the named stage would leak the feature past the gate.
	if got := inputsFromIntegrity("scrape.body", upstream, completed, false); got != apiv1.IntegrityTrusted {
		t.Errorf("integrity of scrape.body under a legacy version = %q, want %q", got, apiv1.IntegrityTrusted)
	}
}

// A TOLERATED failure is forgotten, so a qualified reference to it falls
// through to bare-key resolution rather than reporting "stage produced no
// output".
//
// This is why the walk carries a completed-stage map at all instead of reusing
// its `upstream` results: the engine nils a tolerated failure's Outputs and
// keeps the entry, so the stage is PRESENT-but-empty there while the runner's
// map has it ABSENT — and present-but-empty takes the qualified branch.
func TestToleratedFailureIsForgotten(t *testing.T) {
	completed := newCompletedStages()
	tolerated := apiv1.Task{Name: "scan", ContinueOnError: true}
	completed.RecordCompleted(tolerated, apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{"findings": 3},
	})
	if _, ok := resolveInputsFrom("scan.findings", apiv1.ResultEnvelope{}, completed, true); !ok {
		t.Fatal("a SUCCEEDING tolerated stage must still be addressable")
	}

	completed.RecordCompleted(tolerated, apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Outputs: map[string]interface{}{"findings": 3},
		Error:   &apiv1.ErrorInfo{Code: "scanner_unavailable"},
	})
	upstream := apiv1.ResultEnvelope{Outputs: map[string]interface{}{"scan.findings": "bare"}}
	got, ok := resolveInputsFrom("scan.findings", upstream, completed, true)
	if !ok || got != "bare" {
		t.Errorf("value = %#v (ok=%t), want the bare key %q — a tolerated failure's outputs are discarded, "+
			"and a reference to the forgotten stage falls through", got, ok, "bare")
	}
}

// An unresolvable reference must fail the stage CLOSED, and say which of the
// two things went wrong. The wording is part of the port: a stage-closed
// failure is what an operator reads, and the legacy phrasing is what pre-3.0
// lanes' operators already know.
func TestInputsFromErrorNamesTheRightMiss(t *testing.T) {
	completed := newCompletedStages()
	recordStage(completed, "build", map[string]interface{}{"sha": "abc", "artifact": "x.tar"}, apiv1.IntegrityTrusted)

	qualified := inputsFromError("deploy", "digest", "build.digest", completed, true).Error()
	for _, want := range []string{`stage "build" produced no output "digest"`, "artifact, sha"} {
		if !strings.Contains(qualified, want) {
			t.Errorf("qualified miss = %q, want it to contain %q — a stage that RAN must name what it emitted",
				qualified, want)
		}
	}
	if got, want := len(strings.Split(qualified, "artifact, sha")), 2; got != want {
		t.Errorf("emitted-key list is not sorted deterministically: %q", qualified)
	}

	unknown := inputsFromError("deploy", "digest", "nosuchstage.digest", completed, true).Error()
	if !strings.Contains(unknown, `upstream output "nosuchstage.digest" not found`) {
		t.Errorf("unknown-prefix miss = %q, want the legacy bare-key wording — the value was resolved as a bare key, "+
			"so the message must say so", unknown)
	}

	legacy := inputsFromError("deploy", "digest", "build.digest", completed, false).Error()
	if !strings.Contains(legacy, `upstream output "build.digest" not found`) {
		t.Errorf("legacy-version miss = %q, want the bare-key wording — under a pre-#562 version the value was "+
			"never a stage reference", legacy)
	}
}

// The recorded map must not alias the walk's live result envelope. The walk
// keeps handing the same ResultEnvelope onward, so a shared Outputs map would
// let a later mutation rewrite an already-completed stage's recorded outputs —
// and on a replayed workflow that is a nondeterminism bug, not just a stale
// read.
func TestRecordedStageOutputsAreCopied(t *testing.T) {
	live := map[string]interface{}{"sha": "abc"}
	completed := newCompletedStages()
	recordStage(completed, "build", live, apiv1.IntegrityTrusted)
	live["sha"] = "mutated"

	got, ok := resolveInputsFrom("build.sha", apiv1.ResultEnvelope{}, completed, true)
	if !ok || got != "abc" {
		t.Errorf("value = %#v (ok=%t), want %q — recorded outputs must not alias the live envelope", got, ok, "abc")
	}
}
