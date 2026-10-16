package validate

import (
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

func TestChildWorkflowOriginClosedIdentityContract(t *testing.T) {
	validator := newV(t)
	env := completeInvocationEnvelope()
	env.NestedAgentPolicy, env.ParentPlatformPolicy = nil, nil
	encoded, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON(schemas.Envelope["invocation"], encoded); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing occurrence": func(v map[string]any) { delete(v["childWorkflowOrigin"].(map[string]any), "stageOccurrence") },
		"missing attempt":    func(v map[string]any) { delete(v["childWorkflowOrigin"].(map[string]any), "attemptId") },
		"unbound identity":   func(v map[string]any) { v["childWorkflowOrigin"].(map[string]any)["attemptId"] = "attempt-2" },
		"token":              func(v map[string]any) { v["childWorkflowOrigin"].(map[string]any)["token"] = "forbidden" },
		"policy": func(v map[string]any) {
			v["childWorkflowOrigin"].(map[string]any)["allowedGoobers"] = []string{"coder"}
		},
		"missing owner":    func(v map[string]any) { delete(v, "goober") },
		"missing dispatch": func(v map[string]any) { delete(v, "attempt") },
		"missing boundary": func(v map[string]any) { delete(v, "ownershipBoundary") },
	} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := validator.ValidateJSON(schemas.Envelope["invocation"], data); err == nil {
				t.Fatal("invalid child origin accepted")
			}
		})
	}
}
