package engine

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

// The archived input is the authority after recovery or worker handoff. Neither
// the current daemon environment nor an authored input can change its contract.
func TestPinnedDSLVersionSurvivesRunAndDispatchSerialization(t *testing.T) {
	t.Setenv("GOOBERS_DSL_VERSION", "2.0")
	for _, version := range []string{"", "2.0", "2.1", "3.0", "3.1"} {
		in := RunInput{RunID: "run", WorkflowName: "watch", DSLVersion: version}
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var restored RunInput
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		env := buildInvocation(restored, "watch", "watch", map[string]string{"dslVersion": "2.0"}, nil, apiv1.Limits{}, nil, "")
		if env.DSLVersion != version {
			t.Fatalf("invocation=%q want %q", env.DSLVersion, version)
		}
		attempt := newDispatchAttempt(DispatchStageInput{Envelope: env})
		data, err = json.Marshal(attempt)
		if err != nil {
			t.Fatal(err)
		}
		var worker dispatcher.Attempt
		if err := json.Unmarshal(data, &worker); err != nil {
			t.Fatal(err)
		}
		if worker.DSLVersion != version {
			t.Fatalf("worker=%q want %q", worker.DSLVersion, version)
		}
	}
}
