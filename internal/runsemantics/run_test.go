package runsemantics

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRemediationFailureEvidencePointersSelectTriggeringEvidence(t *testing.T) {
	pointers := []apiv1.ContextPointer{
		{Name: "query-backlog.artifact[0]"},
		{Name: "local-ci.artifact[0]"},
		{Name: "local-ci.artifact[1]"},
		{Name: "review.verdict"},
		{Name: "review.diff"},
	}
	t.Run("CI failure", func(t *testing.T) {
		got := RemediationFailureEvidencePointers(&gate.RepassCause{
			Kind: "stage-failure", Stage: "local-ci",
		}, pointers)
		if names := RequiredContextPointerNames(got); !reflect.DeepEqual(names, []string{
			"local-ci.artifact[0]", "local-ci.artifact[1]",
		}) {
			t.Fatalf("failure evidence = %v, want only local-ci artifacts", names)
		}
	})
	t.Run("review", func(t *testing.T) {
		got := RemediationFailureEvidencePointers(&gate.RepassCause{
			Kind: "reviewer", Gate: "review",
		}, pointers)
		if names := RequiredContextPointerNames(got); !reflect.DeepEqual(names, []string{"review.verdict"}) {
			t.Fatalf("failure evidence = %v, want only review verdict", names)
		}
	})
}

// Unit test for parseTranscriptInputInspection function.
func TestParseTranscriptInputInspection(t *testing.T) {
	tests := []struct {
		name          string
		transcript    string
		required      map[string]struct{}
		wantList      bool
		wantInspected map[string]bool
	}{
		{
			name:          "empty transcript",
			transcript:    "",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      false,
			wantInspected: map[string]bool{},
		},
		{
			name:          "list_inputs only",
			transcript:    `{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      true,
			wantInspected: map[string]bool{},
		},
		{
			name: "list_inputs and read_input for required pointer",
			transcript: `{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-read_input","arguments":{"name":"pointer1"}}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      true,
			wantInspected: map[string]bool{"pointer1": true},
		},
		{
			name: "read_input for unrequired pointer",
			transcript: `{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-read_input","arguments":{"name":"unrequired"}}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      true,
			wantInspected: map[string]bool{},
		},
		{
			name: "grep_input for required pointer",
			transcript: `{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-grep_input","arguments":{"name":"pointer1"}}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      true,
			wantInspected: map[string]bool{"pointer1": true},
		},
		{
			name: "multiple pointers with mixed inspection",
			transcript: `{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-read_input","arguments":{"name":"pointer1"}}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-read_input","arguments":{"name":"pointer2"}}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}, "pointer2": {}, "pointer3": {}},
			wantList:      true,
			wantInspected: map[string]bool{"pointer1": true, "pointer2": true},
		},
		{
			name: "malformed JSON lines are skipped",
			transcript: `not json` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"invalid":json}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-read_input","arguments":{"name":"pointer1"}}}` + "\n",
			required:      map[string]struct{}{"pointer1": {}},
			wantList:      true,
			wantInspected: map[string]bool{"pointer1": true},
		},
		{
			name: "non-assistant role is ignored",
			transcript: `{"role":"user","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n" +
				`{"role":"assistant","tool_call":{"name":"goobers-io-list_inputs"}}` + "\n",
			required:      map[string]struct{}{},
			wantList:      true,
			wantInspected: map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotList, gotInspected := ParseTranscriptInputInspection([]byte(tt.transcript), tt.required)
			if gotList != tt.wantList {
				t.Fatalf("sawListInputs = %v, want %v", gotList, tt.wantList)
			}
			// The function returns true for required pointers that were inspected,
			// and does not include entries for pointers not found
			for key, wantVal := range tt.wantInspected {
				if gotVal, ok := gotInspected[key]; !ok || gotVal != wantVal {
					t.Fatalf("inspected[%q] = %v (ok=%v), want %v", key, gotVal, ok, wantVal)
				}
			}
			// Check that pointers not in wantInspected are also not in gotInspected
			for key := range gotInspected {
				if _, ok := tt.wantInspected[key]; !ok {
					t.Fatalf("unexpected inspected pointer: %q", key)
				}
			}
		})
	}
}

// Unit test for pointerValidationErrorMessage function.
func TestPointerValidationErrorMessage(t *testing.T) {
	tests := []struct {
		name      string
		missing   []string
		sawList   bool
		wantMsg   string
		wantEmpty bool
	}{
		{
			name:      "no missing pointers and list_inputs was called",
			missing:   []string{},
			sawList:   true,
			wantEmpty: true,
		},
		{
			name:    "missing pointers and list_inputs not called",
			missing: []string{"pointer1", "pointer2"},
			sawList: false,
			wantMsg: "required context pointers were not inspected before DEPENDENCY_NOT_MET",
		},
		{
			name:    "missing pointers and list_inputs was called",
			missing: []string{"pointer1", "pointer2"},
			sawList: true,
			wantMsg: "required context pointers were not inspected before DEPENDENCY_NOT_MET",
		},
		{
			name:    "single missing pointer",
			missing: []string{"single-pointer"},
			sawList: false,
			wantMsg: "required context pointers were not inspected before DEPENDENCY_NOT_MET",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := PointerValidationErrorMessage(tt.missing, tt.sawList)
			if tt.wantEmpty && msg != "" {
				t.Fatalf("message = %q, want empty", msg)
			}
			if !tt.wantEmpty && msg == "" {
				t.Fatalf("message is empty, want non-empty")
			}
			if !tt.wantEmpty {
				if !strings.Contains(msg, tt.wantMsg) {
					t.Fatalf("message = %q, want to contain %q", msg, tt.wantMsg)
				}
				// Verify all missing pointers are named in the message
				for _, pointer := range tt.missing {
					if !strings.Contains(msg, pointer) {
						t.Fatalf("message = %q, want to contain pointer %q", msg, pointer)
					}
				}
				// Verify list_inputs status is included
				if !strings.Contains(msg, "list_inputs") {
					t.Fatalf("message = %q, want to contain list_inputs status", msg)
				}
			}
		})
	}
}

// agenticGateMachine mirrors fixtureMachine but with an AGENTIC reviewer gate
// (goober "reviewer") instead of an automated one, for exercising #294's
// gate-envelope capability sourcing.
func agenticGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          workflow.TerminalComplete,
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "agentic-gate-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile agentic gate machine: %v", err)
	}
	return m
}
