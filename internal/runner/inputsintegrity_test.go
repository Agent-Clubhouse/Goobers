package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runsemantics"
)

// A stage declaring no minimum keeps working unchanged — this must not become a
// tax on every workflow that has never opted into integrity.
func TestInputsFromUngatedWhenNoMinimumDeclared(t *testing.T) {
	task := apiv1.Task{Name: "ci-poll", InputsFrom: map[string]string{"prNumber": "open-pr.prNumber"}}
	completed := runsemantics.StageOutputs{"open-pr": {Outputs: map[string]any{"prNumber": 7}}}
	grades := resolvedInputGrades(task, nil, apiv1.ResultEnvelope{}, completed, nil)
	if err := apiv1.ValidateResolvedInputIntegrity(grades, task.MinimumIntegrity); err != nil {
		t.Fatalf("ungated task was refused: %v", err)
	}
}

// Provenance flows with the data: an agent that reads unapproved text cannot
// launder it into a maintainer-graded output for the next stage.
func TestProducedIntegrityDecaysToWeakestInput(t *testing.T) {
	agentic := apiv1.Task{Name: "implement", Type: apiv1.TaskAgentic}
	got := producedIntegrity(agentic, &apiv1.BacklogItem{Integrity: apiv1.IntegrityMaintainer}, nil,
		map[string]apiv1.Integrity{"body": apiv1.IntegrityUnapproved})
	if got != apiv1.IntegrityUnapproved {
		t.Errorf("produced = %q, want %q — unapproved input must not be laundered", got, apiv1.IntegrityUnapproved)
	}

	// Agentic output stays distinguishable as derived, which still satisfies a
	// maintainer minimum (Grade.Meets), so this does not break normal flows.
	got = producedIntegrity(agentic, &apiv1.BacklogItem{Integrity: apiv1.IntegrityMaintainer}, nil, nil)
	if got != apiv1.IntegrityDerived {
		t.Errorf("produced = %q, want %q", got, apiv1.IntegrityDerived)
	}
	if !got.Meets(apiv1.IntegrityMaintainer) {
		t.Error("derived output must still satisfy a maintainer minimum")
	}

	// A deterministic stage with no graded input ran purely from operator config.
	if got := producedIntegrity(apiv1.Task{Name: "pr-select"}, nil, nil, nil); got != apiv1.IntegrityTrusted {
		t.Errorf("produced = %q, want %q", got, apiv1.IntegrityTrusted)
	}
}

// #2979: a grade the executor stamped on its own envelope survives the
// input-derived grade. A deterministic stage with no graded input (so
// producedIntegrity alone says trusted) whose executor stamped unapproved must
// finish unapproved, and a downstream stage requiring derived must refuse it.
func TestStageResultIntegrityKeepsExecutorStampedGrade(t *testing.T) {
	telemetry := apiv1.Task{Name: "query"}
	produced := producedIntegrity(telemetry, nil, nil, nil)
	if produced != apiv1.IntegrityTrusted {
		t.Fatalf("precondition: produced = %q, want %q", produced, apiv1.IntegrityTrusted)
	}
	got := runsemantics.StageResultIntegrity(apiv1.IntegrityUnapproved, produced)
	if got != apiv1.IntegrityUnapproved {
		t.Fatalf("stage integrity = %q, want the executor's %q", got, apiv1.IntegrityUnapproved)
	}

	completed := runsemantics.StageOutputs{}
	completed.Record("query", map[string]any{"telemetryValue": "42"}, got)
	consumer := apiv1.Task{
		Name:             "consume",
		MinimumIntegrity: apiv1.IntegrityDerived,
		InputsFrom:       map[string]string{"v": "query.telemetryValue"},
	}
	grades := map[string]apiv1.Integrity{
		"v": runsemantics.InputsFromIntegrity("query.telemetryValue", apiv1.ResultEnvelope{}, completed, true),
	}
	if err := apiv1.ValidateResolvedInputIntegrity(grades, consumer.MinimumIntegrity); err == nil {
		t.Fatal("unapproved executor output was admitted at the derived tier")
	}
}
