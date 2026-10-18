package main

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// TestPRCommentWatchReferenceWorkflowCompiles keeps the reference
// pr-comment-watch workflow honest (#6134): it opts in to disarming landing
// PRs, so it must declare the disarm-landing policy action that
// blockUnansweredFeedback "true" prescribes, and compile.
func TestPRCommentWatchReferenceWorkflowCompiles(t *testing.T) {
	raw, err := os.ReadFile("testdata/pr-comment-watch.workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	task := w.Spec.Tasks[0]
	if task.Inputs["blockUnansweredFeedback"] != "true" || !strings.Contains(strings.Join(task.PolicyActions, ","), "disarm-landing") {
		t.Fatalf("reference task = %+v, want blockUnansweredFeedback \"true\" with disarm-landing declared", task)
	}
	if _, err := workflow.Compile(workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}); err != nil {
		t.Fatalf("compile reference workflow: %v", err)
	}
	task.PolicyActions = []string{"label-issue"}
	w.Spec.Tasks[0] = task
	if _, err := workflow.Compile(workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}); err == nil {
		t.Fatal("compiled without disarm-landing declared; blockUnansweredFeedback \"true\" must prescribe it")
	}
	// Every spelling the stage's strconv.ParseBool accepts as true enables
	// the cancel, so every one must prescribe the action.
	for _, truthy := range []string{"1", "t", "True", " TRUE "} {
		task.Inputs["blockUnansweredFeedback"] = truthy
		w.Spec.Tasks[0] = task
		if _, err := workflow.Compile(workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}); err == nil {
			t.Fatalf("blockUnansweredFeedback %q compiled without disarm-landing declared", truthy)
		}
	}
	// disarm-landing requires the credential the cancel runs under.
	task.Inputs["blockUnansweredFeedback"] = "true"
	task.PolicyActions = []string{"label-issue", "disarm-landing"}
	task.Capabilities = []string{"github:issues:write"}
	w.Spec.Tasks[0] = task
	if _, err := workflow.Compile(workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}); err == nil {
		t.Fatal("disarm-landing compiled without github:pr:write")
	}
}
