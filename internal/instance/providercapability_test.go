package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

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

	got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderGitHub)
	want := []providers.Capability{
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

// TestBacklogQueryAloneDerivesNoProviderCapabilityRequirement pins the fix
// for CONF-6's original over-broad requirement: backlog-query's
// HasOpenWorkItemBlocker call only fires when a work item's BlockedByCount
// != 0, and only GitHub's ListWorkItems ever sets that field, so requiring
// CapBacklogBlockers for every backlog-query-using workflow refused
// backlog providers (e.g. ADO) over a codepath they can never reach. A
// backlog-query-only workflow must derive zero provider-capability
// requirements and never be refused at config-load regardless of backlog
// provider.
func TestBacklogQueryAloneDerivesNoProviderCapabilityRequirement(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("query", "backlog-query")},
	}}
	wf.Name = "implementation"

	if got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderGitHub); len(got) != 0 {
		t.Fatalf("required = %v, want none", got)
	}

	g := adoGaggle("web")
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{g}, Workflows: []apiv1.Workflow{wf}}
	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("unexpected error: %v", err)
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

	if got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderGitHub); got != nil {
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

	got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderGitHub)
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

// #2061/#2179: apply-verdict on ADO used to be refused at config load because
// its derived requirement was pr.review.submit, which ADO deliberately does not
// declare — rejecting a lane cmd/goobers/applyverdict.go implements through
// PR status + thread comment and ado-provider-parity.md documents. The stage
// now derives pr.status.publish on ADO, which ADO does declare.
func TestCheckProviderCapabilityRequirementsAdmitsADOApplyVerdict(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("apply", "apply-verdict")},
	}}
	wf.Name = "merge-review"
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{adoGaggle("web")}, Workflows: []apiv1.Workflow{wf}}

	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("ADO apply-verdict must validate: %v", err)
	}

	got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderADO)
	if len(got) != 1 || got[0] != providers.CapPRStatusPublish {
		t.Errorf("ADO derivation = %v, want [%s]", got, providers.CapPRStatusPublish)
	}
	// The GitHub derivation is unchanged: the override replaces the default
	// only for the provider it names.
	if got := WorkflowRequiredProviderCapabilitiesFor(wf, providers.ProviderGitHub); len(got) != 1 || got[0] != providers.CapPRReviewSubmit {
		t.Errorf("GitHub derivation = %v, want [%s]", got, providers.CapPRReviewSubmit)
	}
}

// A genuine ADO gap must still refuse. gather-review-threads derives
// pr.review.threads, which ADO does not declare and for which no ADO path
// exists — the shape apply-verdict was wrongly lumped in with.
func TestCheckProviderCapabilityRequirementsRejectsRealADOGap(t *testing.T) {
	wf := apiv1.Workflow{Spec: apiv1.WorkflowSpec{
		Gaggle: "web",
		Tasks:  []apiv1.Task{deterministicStage("threads", "gather-review-threads")},
	}}
	wf.Name = "implementation"
	set := &ConfigSet{Gaggles: []apiv1.Gaggle{adoGaggle("web")}, Workflows: []apiv1.Workflow{wf}}

	err := CheckProviderCapabilityRequirements(set)
	if err == nil {
		t.Fatal("expected error — ADO does not declare pr.review.threads")
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

// TestShippedMergeReviewWorkflowValidatesOnADO is the #2061 acceptance
// evidence in test form: the merge-review workflow this repository actually
// ships must be configurable on an Azure DevOps gaggle.
//
// It could not be, before #2061's fix. ado-provider-parity.md documents an
// end-to-end ADO merge-review lane and cmd/goobers/applyverdict.go implements
// it, but apply-verdict derived pr.review.submit for every provider, ADO does
// not declare it, and config load rejected the whole workflow. The documented
// lane, the conformance design, and the shipped workflow did not describe one
// executable product.
//
// This reads the real reference-workflows definition rather than a fixture, so
// a future stage added to that lane that ADO cannot serve fails here instead of
// on a customer's instance.
func TestShippedMergeReviewWorkflowValidatesOnADO(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "merge-review.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped merge-review workflow: %v", err)
	}
	var wf apiv1.Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("unmarshal shipped merge-review workflow: %v", err)
	}
	if len(wf.Spec.Tasks) == 0 {
		t.Fatal("shipped merge-review workflow has no tasks; the check would pass vacuously")
	}
	wf.Spec.Gaggle = "web"

	set := &ConfigSet{Gaggles: []apiv1.Gaggle{adoGaggle("web")}, Workflows: []apiv1.Workflow{wf}}
	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("the shipped merge-review workflow must be configurable on ADO (#2061): %v", err)
	}

	// The same workflow must still validate on GitHub, where apply-verdict
	// derives the native-review capability instead.
	set.Gaggles = []apiv1.Gaggle{githubGaggle("web")}
	if err := CheckProviderCapabilityRequirements(set); err != nil {
		t.Fatalf("the shipped merge-review workflow must remain configurable on GitHub: %v", err)
	}
}
