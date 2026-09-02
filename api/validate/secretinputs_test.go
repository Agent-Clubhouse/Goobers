package validate

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// secretinputs_test.go covers SEC001 (#2931): stage inputs are
// history-resident, so a credential-shaped literal in `inputs:` is reported at
// author time — the one moment it can still be removed before it reaches a
// durable Temporal history that cannot be rewritten.

// leakedTokenLiteral is shaped exactly like a GitHub personal access token but
// is not one: the pattern net keys on shape alone, and the test must not carry
// a real credential to exercise it.
const leakedTokenLiteral = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

func secretInputFindings(t *testing.T, task apiv1.Task) []Issue {
	t.Helper()
	ix := newIndex()
	ix.gaggles["example"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{}}
	workflow := apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "example-workflow"},
		DSLVersion: supportmatrix.NextDSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "example", Start: task.Name, Tasks: []apiv1.Task{task},
		},
	}
	report := &Report{}
	ix.checkWorkflow(report, workflow, "workflow.yaml", false)

	var found []Issue
	for _, issue := range report.Issues {
		if issue.Code == WarningSecretShapedInput {
			found = append(found, issue)
		}
	}
	return found
}

func TestSecretShapedInputCodeStable(t *testing.T) {
	if got, want := WarningSecretShapedInput, WarningCode("SEC001"); got != want {
		t.Fatalf("WarningSecretShapedInput = %q, want stable code %q", got, want)
	}
	for _, other := range []WarningCode{WarningSubprocessTimeout, WarningZeroMaxRunsPerHour, WarningConnectionRefUnhonored} {
		if WarningSecretShapedInput == other {
			t.Fatalf("WarningSecretShapedInput duplicates %q", other)
		}
	}
}

func TestSecretShapedInputWarningWiredIntoValidate(t *testing.T) {
	tests := []struct {
		name  string
		task  apiv1.Task
		want  int
		names []string
	}{
		{
			name: "token literal in inputs warns",
			task: apiv1.Task{
				Name: "publish", Type: apiv1.TaskDeterministic, Goal: "Publish.",
				Run:    &apiv1.DeterministicRun{Command: []string{"publish"}},
				Inputs: map[string]string{"token": leakedTokenLiteral},
			},
			want:  1,
			names: []string{"token"},
		},
		{
			name: "every offending key is reported, deterministically",
			task: apiv1.Task{
				Name: "publish", Type: apiv1.TaskDeterministic, Goal: "Publish.",
				Run: &apiv1.DeterministicRun{Command: []string{"publish"}},
				Inputs: map[string]string{
					"zeta":  "Authorization: Bearer " + leakedTokenLiteral,
					"alpha": "AKIA0123456789ABCDEF",
					"clean": "https://example.com/repo",
				},
			},
			want:  2,
			names: []string{"alpha", "zeta"},
		},
		{
			name: "experiment arm variant overlays the same envelope inputs blob",
			task: apiv1.Task{
				Name: "publish", Type: apiv1.TaskDeterministic, Goal: "Publish.",
				Run: &apiv1.DeterministicRun{Command: []string{"publish"}},
				Experiment: &apiv1.BanditExperiment{Arms: []apiv1.BanditArm{
					{Name: "control", Variant: map[string]string{"mode": "fast"}, GateLevel: 1},
					{Name: "treatment", Variant: map[string]string{"token": leakedTokenLiteral}, GateLevel: 1},
				}},
			},
			want:  1,
			names: []string{"treatment"},
		},
		{
			name: "ordinary inputs are clean — the check must not tax normal configs",
			task: apiv1.Task{
				Name: "publish", Type: apiv1.TaskDeterministic, Goal: "Publish.",
				Run: &apiv1.DeterministicRun{Command: []string{"publish"}},
				Inputs: map[string]string{
					"credentialRef": "credential:github:pr:write",
					"issue":         "2931",
					"branch":        "goobers/implementation/abc123",
					"empty":         "",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			found := secretInputFindings(t, tc.task)
			if len(found) != tc.want {
				t.Fatalf("SEC001 findings = %d (%v), want %d", len(found), found, tc.want)
			}
			for i, issue := range found {
				if issue.Severity != Warning {
					t.Errorf("finding %d severity = %q, want warning", i, issue.Severity)
				}
				if !strings.Contains(issue.Message, tc.names[i]) {
					t.Errorf("finding %d message %q must name %q", i, issue.Message, tc.names[i])
				}
				if !strings.Contains(issue.Message, "#2931") || !strings.Contains(issue.Message, "history-resident") {
					t.Errorf("finding %d message %q must cite #2931 and explain history residency", i, issue.Message)
				}
			}
		})
	}
}

// TestSecretShapedInputFindingNeverEchoesTheValue is the property that makes
// this warning safe to emit: the report lands in logs, CI annotations and the
// JSON report, so a finding that quoted the literal would copy the suspected
// credential into three more places than the config already put it.
func TestSecretShapedInputFindingNeverEchoesTheValue(t *testing.T) {
	found := secretInputFindings(t, apiv1.Task{
		Name: "publish", Type: apiv1.TaskDeterministic, Goal: "Publish.",
		Run:    &apiv1.DeterministicRun{Command: []string{"publish"}},
		Inputs: map[string]string{"token": leakedTokenLiteral},
	})
	if len(found) != 1 {
		t.Fatalf("SEC001 findings = %v, want exactly one", found)
	}
	if strings.Contains(found[0].Message, leakedTokenLiteral) {
		t.Fatalf("finding %q echoes the secret-shaped literal", found[0].Message)
	}
}
