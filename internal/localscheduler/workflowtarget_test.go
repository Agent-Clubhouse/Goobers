package localscheduler

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResolveWorkflowTarget(t *testing.T) {
	catalog := []WorkflowIdentity{
		{Gaggle: "beta", Workflow: "implement"},
		{Gaggle: "alpha", Workflow: "review"},
		{Gaggle: "alpha", Workflow: "implement"},
		{Gaggle: "beta", Workflow: "triage"},
	}
	for _, tc := range []struct {
		name, gaggle, workflow string
		wantUnknown            bool
		wantMessage            string
	}{
		{name: "unqualified unique", workflow: "review"},
		{name: "qualified", gaggle: "beta", workflow: "implement"},
		{
			name: "unqualified unknown lists every gaggle", workflow: "list", wantUnknown: true,
			wantMessage: `no workflow named "list"; available workflows: alpha/implement, alpha/review, beta/implement, beta/triage`,
		},
		{
			name: "qualified unknown lists only that gaggle", gaggle: "alpha", workflow: "triage", wantUnknown: true,
			wantMessage: `no workflow named "triage" in gaggle "alpha"; available workflows: implement, review`,
		},
		{
			name: "unknown gaggle lists the whole catalog", gaggle: "gamma", workflow: "implement", wantUnknown: true,
			wantMessage: `no workflow named "implement" in gaggle "gamma"; no gaggle named "gamma" is configured; available workflows: alpha/implement, alpha/review, beta/implement, beta/triage`,
		},
		{
			name: "unqualified ambiguous", workflow: "implement",
			wantMessage: `workflow "implement" is ambiguous; candidate gaggles: alpha, beta`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ResolveWorkflowTarget(catalog, tc.gaggle, tc.workflow)
			if tc.wantMessage == "" {
				if err != nil {
					t.Fatalf("err = %v, want resolved", err)
				}
				return
			}
			var unknown *UnknownWorkflowError
			if err == nil || errors.As(err, &unknown) != tc.wantUnknown || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("err = %v, want unknown=%v containing %q", err, tc.wantUnknown, tc.wantMessage)
			}
		})
	}
}

func TestUnknownWorkflowErrorBoundsListedCatalog(t *testing.T) {
	var catalog []WorkflowIdentity
	for i := range maxListedWorkflows + 3 {
		catalog = append(catalog, WorkflowIdentity{Gaggle: "g", Workflow: fmt.Sprintf("w%02d", i)})
	}
	err := ResolveWorkflowTarget(catalog, "g", "missing")
	if err == nil || !strings.HasSuffix(err.Error(), "w19 (and 3 more)") {
		t.Fatalf("err = %v, want a bounded list with the remainder counted", err)
	}
}
