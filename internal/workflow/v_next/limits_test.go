package vnext

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestTaskLimitsPopulatesInvocationLimits(t *testing.T) {
	task := apiv1.Task{
		TimeoutSeconds: 90,
		Limits: &apiv1.Limits{
			MaxDurationSeconds: 30,
			MaxTokens:          1200,
			MaxCostUSD:         2.5,
		},
	}
	got := TaskLimits(task)
	if got.MaxDurationSeconds != 90 || got.MaxTokens != 1200 || got.MaxCostUSD != 2.5 {
		t.Fatalf("TaskLimits = %+v, want timeout override with token and cost limits preserved", got)
	}
}

func TestGateLimitsUsesSelectedEvaluator(t *testing.T) {
	automated := GateLimits(apiv1.Gate{
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{TimeoutSeconds: 15},
		Agentic:   &apiv1.AgenticGate{TimeoutSeconds: 99},
	})
	if automated.MaxDurationSeconds != 15 {
		t.Fatalf("automated gate duration = %d, want 15", automated.MaxDurationSeconds)
	}

	agentic := GateLimits(apiv1.Gate{
		Evaluator: apiv1.EvaluatorAgentic,
		Automated: &apiv1.AutomatedGate{TimeoutSeconds: 15},
		Agentic:   &apiv1.AgenticGate{TimeoutSeconds: 99},
	})
	if agentic.MaxDurationSeconds != 99 {
		t.Fatalf("agentic gate duration = %d, want 99", agentic.MaxDurationSeconds)
	}
}
