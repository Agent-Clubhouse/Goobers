package instance

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func githubGaggle(name string) apiv1.Gaggle {
	return gaggle(name, apiv1.GaggleSpec{
		Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/web"},
	})
}

func adoGaggle(name string) apiv1.Gaggle {
	return gaggle(name, apiv1.GaggleSpec{
		Project: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "web", Name: "web"},
		Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "acme/web"},
	})
}

func deterministicStage(name, verb string) apiv1.Task {
	return apiv1.Task{
		Name: name, Type: apiv1.TaskDeterministic, Goal: "g",
		Run: &apiv1.DeterministicRun{Command: []string{"goobers", verb}},
	}
}

func TestWorkflowRequiredProviderCapabilitiesDerivesFromStages(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks: []apiv1.Task{
			deterministicStage("merge", "merge-pr"),
			deterministicStage("query", "backlog-query"),
			{Name: "implement", Type: apiv1.TaskAgentic, Goal: "g", Goober: "coder"},
		},
	}}
	wf.Name = "implementation"

	got := WorkflowRequiredProviderCapabilities(wf)
	want := []providers.Capability{
		providers.CapBacklogBlockers,
		providers.CapBranchDelete,
		providers.CapPRCompare,
		providers.CapPRLandingDetectPolicy,
		providers.CapPRLandingEnqueue,
		providers.CapPRMerge,
	}
	if len(got) != len(want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("required = %v, want %v", got, want)
		}
	}
}

func TestWorkflowRequiredProviderCapabilitiesEmptyForPlainWorkflow(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks: []apiv1.Task{
			deterministicStage("query", "backlog-dedupe"),
			{Name: "implement", Type: apiv1.TaskAgentic, Goal: "g", Goober: "coder"},
		},
	}}
	wf.Name = "plain"

	if got := WorkflowRequiredProviderCapabilities(wf); got != nil {
		t.Fatalf("required = %v, want nil (no optional capability used)", got)
	}
}

func TestWorkflowRequiredProviderCapabilitiesExplicitOverridesDerivation(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("merge", "merge-pr")},
		Requires: &apiv1.WorkflowRequirements{
			Capabilities: []string{string(providers.CapPRReviewThreads)},
		},
	}}
	wf.Name = "custom"

	got := WorkflowRequiredProviderCapabilities(wf)
	if len(got) != 1 || got[0] != providers.CapPRReviewThreads {
		t.Fatalf("required = %v, want an explicit override replacing the merge-pr derivation", got)
	}
}

func TestCheckProviderCapabilityRequirementsPassesOnGitHub(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("merge", "merge-pr")},
	}}
	wf.Name = "implementation"
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{githubGaggle("web")}, Workflows: []apiv1.Workflow{wf}}

	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProviderCapabilityRequirementsRejectsADOGapOnLanding(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("merge", "merge-pr")},
	}}
	wf.Name = "implementation"
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{adoGaggle("web")}, Workflows: []apiv1.Workflow{wf}}

	err := CheckProviderCapabilityRequirements(set)
	if err == nil {
		t.Fatal("expected error — ADO does not declare pr.merge/pr.landing.enqueue/pr.landing.detect-policy/pr.compare/branch.delete")
	}
	if !strings.Contains(err.Error(), "implementation") || !strings.Contains(err.Error(), "ado") {
		t.Errorf("error must name the workflow and the provider: %v", err)
	}
}

func TestCheckProviderCapabilityRequirementsChecksBacklogProviderSeparately(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("query", "backlog-query")},
	}}
	wf.Name = "implementation"
	// Project on GitHub (has pr.* landing surfaces), Backlog on Gitea — a
	// mixed gaggle. Gitea declares backlog.blockers (a real check, unlike
	// ADO's fail-open stub CONF-5/#2078 deleted), so this must still pass.
	g := githubGaggle("web")
	g.Spec.Backlog = apiv1.BacklogRef{Provider: apiv1.ProviderGitea, Project: "acme/web"}
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{g}, Workflows: []apiv1.Workflow{wf}}

	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProviderCapabilityRequirementsPassesWithNoRequirements(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goal: "g", Goober: "coder"}},
	}}
	wf.Name = "plain"
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{adoGaggle("web")}, Workflows: []apiv1.Workflow{wf}}

	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckProviderCapabilityRequirementsSkipsDanglingGaggleReference(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "ghost",
		Tasks:  []apiv1.Task{deterministicStage("merge", "merge-pr")},
	}}
	wf.Name = "implementation"
	set := &ConfigSet{Workflows: []apiv1.Workflow{wf}}

	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("unexpected error (dangling gaggle ref is api/validate's concern, not this check's): %v", err)
	}
}
