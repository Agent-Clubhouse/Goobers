package runcontrol

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolvePrecedenceAndPartialInheritance(t *testing.T) {
	gaggle := &apiv1.RunControls{MaxRepasses: 5, MaxRunDuration: "8h"}
	workflow := &apiv1.RunControls{StalledRunTimeout: "3h"}
	got, err := Resolve(
		apiv1.RunControls{MaxRepasses: 2, StalledRunTimeout: "30m"},
		gaggle,
		workflow,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRepasses != 5 || got.StalledRunTimeout != 3*time.Hour || got.MaxRunDuration != 8*time.Hour {
		t.Fatalf("Resolve = %+v, want maxRepasses=5 stalledRunTimeout=3h maxRunDuration=8h", got)
	}
}

func TestResolveDefaults(t *testing.T) {
	got, err := Resolve(apiv1.RunControls{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRepasses != DefaultMaxRepasses || got.StalledRunTimeout != DefaultStalledRunTimeout || got.MaxRunDuration != 0 {
		t.Fatalf("Resolve defaults = %+v", got)
	}
}

func TestResolveRejectsInvalidDurationAtItsScope(t *testing.T) {
	_, err := Resolve(apiv1.RunControls{}, &apiv1.RunControls{StalledRunTimeout: "later"}, nil)
	if err == nil || !strings.Contains(err.Error(), "gaggle.spec.runControls.stalledRunTimeout") {
		t.Fatalf("Resolve error = %v, want gaggle run-control path", err)
	}
}

func TestResolveRejectsInvalidMaxRunDurationAtItsScope(t *testing.T) {
	_, err := Resolve(apiv1.RunControls{}, nil, &apiv1.RunControls{MaxRunDuration: "forever"})
	if err == nil || !strings.Contains(err.Error(), "workflow.spec.runControls.maxRunDuration") {
		t.Fatalf("Resolve error = %v, want workflow max-run-duration path", err)
	}
}

// TestValidateDurationDiagnostics pins the author-facing contract for the two
// duration-typed runControls fields: the diagnostic must name the field path,
// the offending value, and the expected form, and must not leak the raw Go
// parse error ("time: invalid duration ...") through the DSL surface.
func TestValidateDurationDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		controls apiv1.RunControls
		want     string // required error substring; "" means Validate must pass
	}{
		{name: "stalledRunTimeout valid", controls: apiv1.RunControls{StalledRunTimeout: "45m"}},
		{name: "stalledRunTimeout empty inherits", controls: apiv1.RunControls{}},
		{
			name:     "stalledRunTimeout invalid syntax",
			controls: apiv1.RunControls{StalledRunTimeout: "sweepprobe"},
			want:     `spec.runControls.stalledRunTimeout "sweepprobe" is not a valid duration; use Go duration syntax, e.g. "45m" or "2h"`,
		},
		{name: "maxRunDuration valid", controls: apiv1.RunControls{MaxRunDuration: "2h"}},
		{name: "maxRunDuration empty inherits", controls: apiv1.RunControls{MaxRunDuration: ""}},
		{
			// A bare number is the likely author mistake: Go requires a unit.
			name:     "maxRunDuration invalid syntax",
			controls: apiv1.RunControls{MaxRunDuration: "45"},
			want:     `spec.runControls.maxRunDuration "45" is not a valid duration; use Go duration syntax, e.g. "45m" or "2h"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate("spec.runControls", tc.controls)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want substring %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "time: invalid duration") {
				t.Fatalf("Validate = %v leaks the raw Go parse error", err)
			}
		})
	}
}

// TestApplyRejectsInvalidDurationInsteadOfZero proves the fail-open assignment
// path is closed: apply used to discard ParseDuration errors, so an invalid
// value silently became 0 — for maxRunDuration, an unlimited run. Resolve's
// Validate call normally screens apply's input, so exercise apply directly to
// show the guard fires without that screen.
func TestApplyRejectsInvalidDurationInsteadOfZero(t *testing.T) {
	effective := Effective{StalledRunTimeout: DefaultStalledRunTimeout}
	err := effective.apply("workflow.spec.runControls", apiv1.RunControls{StalledRunTimeout: "soon"})
	if err == nil || !strings.Contains(err.Error(), `workflow.spec.runControls.stalledRunTimeout "soon"`) {
		t.Fatalf("apply = %v, want stalledRunTimeout diagnostic", err)
	}
	if effective.StalledRunTimeout != DefaultStalledRunTimeout {
		t.Fatalf("apply overwrote StalledRunTimeout with %s despite erroring", effective.StalledRunTimeout)
	}
	err = effective.apply("workflow.spec.runControls", apiv1.RunControls{MaxRunDuration: "unbounded"})
	if err == nil || !strings.Contains(err.Error(), `workflow.spec.runControls.maxRunDuration "unbounded"`) {
		t.Fatalf("apply = %v, want maxRunDuration diagnostic", err)
	}
	if err := effective.apply("workflow.spec.runControls", apiv1.RunControls{MaxRunDuration: "2h"}); err != nil || effective.MaxRunDuration != 2*time.Hour {
		t.Fatalf("apply valid override = %+v, %v; want maxRunDuration=2h", effective, err)
	}
}

func TestMaxRepassesForGate(t *testing.T) {
	gate := apiv1.Gate{MaxRepasses: 1}
	if got := MaxRepassesForGate(gate, 7); got != 1 {
		t.Fatalf("MaxRepassesForGate = %d, want gate override 1", got)
	}
	gate.MaxRepasses = 0
	if got := MaxRepassesForGate(gate, 7); got != 7 {
		t.Fatalf("MaxRepassesForGate inherited = %d, want 7", got)
	}
}

func TestValidateWorkflowRejectsHumanGateRepassBudget(t *testing.T) {
	err := ValidateWorkflow(apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
		Name:        "approval",
		Evaluator:   apiv1.EvaluatorHuman,
		MaxRepasses: 1,
	}}})
	if err == nil || !strings.Contains(err.Error(), "only valid for automated or agentic") {
		t.Fatalf("ValidateWorkflow error = %v", err)
	}
}

func TestValidatePinnedRequiresCompletePolicy(t *testing.T) {
	if err := ValidatePinned(nil); err != nil {
		t.Fatalf("legacy nil controls: %v", err)
	}
	if err := ValidatePinned(&apiv1.RunControls{MaxRepasses: 3}); err == nil ||
		!strings.Contains(err.Error(), "stalledRunTimeout is required") {
		t.Fatalf("partial pinned controls error = %v", err)
	}
}
