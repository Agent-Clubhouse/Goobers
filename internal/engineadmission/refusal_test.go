package engineadmission

import (
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestPlacedDefinitionRefusalsPropagateAndAggregate(t *testing.T) {
	plain := errors.New("plain refusal")
	feature := &UnsupportedFeaturesError{
		Workflow: "flow",
		Refusals: []*UnsupportedFeatureError{{
			Feature: "task.outbox", Stage: "agent", Err: ErrOutboxUnsupported,
		}},
	}
	spec := apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
		Name: "agent", Type: apiv1.TaskAgentic,
		Limits: &apiv1.Limits{MaxTokens: 1},
	}}}
	remote := []dispatcher.PinnedPlacement{{Stage: "agent"}}
	for _, tc := range []struct {
		name    string
		refusal error
		wantLen int
		wantErr error
	}{
		{name: "unsupported features aggregate", refusal: feature, wantLen: 2},
		{name: "plain error propagates", refusal: plain, wantErr: plain},
		{name: "nil adds placement refusal", wantLen: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := placedDefinitionRefusals(
				"flow", spec, remote,
				func(string, apiv1.WorkflowSpec) error { return tc.refusal },
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && len(got) != tc.wantLen {
				t.Fatalf("refusals = %+v, want %d", got, tc.wantLen)
			}
		})
	}
}

// Human gates run on the engine (#7760): they add no refusal, while the
// remaining features and remote limits still aggregate.
func TestRefusePlacedDefinitionAggregatesFeatureAndRemoteLimitsNotHumanGates(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Tasks: []apiv1.Task{{
			Name: "agent", Type: apiv1.TaskAgentic,
			Outbox: []string{"result.json"}, Limits: &apiv1.Limits{MaxTokens: 1},
		}},
		Gates: []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman}},
	}
	err := RefusePlacedDefinition("flow", spec, []dispatcher.PinnedPlacement{{Stage: "agent"}})
	var refused *UnsupportedFeaturesError
	if !errors.As(err, &refused) || len(refused.Refusals) != 2 {
		t.Fatalf("refusal = %#v, want outbox and remote limits", err)
	}
	for _, refusal := range refused.Refusals {
		if refusal.Feature == "gate.evaluator.human" {
			t.Fatalf("human gate refused on the engine: %+v", refusal)
		}
	}
}
