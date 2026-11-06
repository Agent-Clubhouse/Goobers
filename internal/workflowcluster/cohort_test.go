package workflowcluster

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cohortWorkflow() apiv1.Workflow {
	return apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Triggers: []apiv1.Trigger{{
				Type:     apiv1.TriggerWebhook,
				Events:   []string{"pull_request", "issues"},
				Selector: map[string]string{"private-routing-text": "ignored"},
			}},
			Tasks: []apiv1.Task{{
				Name:         "implement",
				Type:         apiv1.TaskAgentic,
				Goal:         "private goal text",
				Capabilities: []string{"repo:push", "provider:pr:write"},
			}},
		},
	}
}

func TestCohortIsDeterministicAndExcludesNamesAndFreeText(t *testing.T) {
	first := cohortWorkflow()
	label := Cohort(first)
	if !strings.HasPrefix(label, "auto:") {
		t.Fatalf("cohort = %q, want auto digest", label)
	}

	renamed := first.DeepCopy()
	renamed.Name = "renamed"
	renamed.Spec.Gaggle = "other"
	renamed.Spec.Tasks[0].Name = "build"
	renamed.Spec.Tasks[0].Goal = "different private text"
	renamed.Spec.Triggers[0].Events = []string{"issues", "pull_request"}
	renamed.Spec.Triggers[0].Selector = map[string]string{"another-secret": "also ignored"}
	if got := Cohort(*renamed); got != label {
		t.Fatalf("renamed/free-text cohort = %q, want %q", got, label)
	}

	changed := first.DeepCopy()
	changed.Spec.Tasks[0].Capabilities = append(changed.Spec.Tasks[0].Capabilities, "repo:read")
	if got := Cohort(*changed); got == label {
		t.Fatalf("structured capability change kept cohort %q", got)
	}
}

func TestCohortCLIValidationKeepsBehaviorAndFleetModesSeparate(t *testing.T) {
	for _, args := range []cliArgs{
		{features: featuresBehavior, cohort: "review", minSize: 3, maxRuns: 1, minSamples: 1},
		{features: featuresStructural, cohort: "review", fleetDir: "snapshots", minSize: 3, maxRuns: 1, minSamples: 1},
	} {
		if !args.invalid() {
			t.Fatalf("args = %+v, want invalid", args)
		}
	}
}

func TestCohortUsesExplicitTaskClass(t *testing.T) {
	workflow := cohortWorkflow()
	workflow.Labels = map[string]string{TaskClassLabel: "review"}
	if got := Cohort(workflow); got != "review" {
		t.Fatalf("cohort = %q, want explicit task class", got)
	}
}

func TestCohortDistinguishesPolicyActionSideEffects(t *testing.T) {
	readOnly := cohortWorkflow()
	readOnlyLabel := Cohort(readOnly)

	commenting := readOnly.DeepCopy()
	commenting.Spec.Tasks[0].PolicyActions = []string{"comment-on-issue"}
	commentingLabel := Cohort(*commenting)
	if commentingLabel == readOnlyLabel {
		t.Fatalf("read-only and mutating workflows share cohort %q", readOnlyLabel)
	}

	merging := readOnly.DeepCopy()
	merging.Spec.Tasks[0].PolicyActions = []string{"merge-pull-request"}
	if got := Cohort(*merging); got == commentingLabel {
		t.Fatalf("different side-effect sets share cohort %q", got)
	}

	reordered := readOnly.DeepCopy()
	reordered.Spec.Tasks[0].PolicyActions = []string{"merge-pull-request", "comment-on-issue"}
	multipleLabel := Cohort(*reordered)
	reordered.Spec.Tasks[0].PolicyActions = []string{"comment-on-issue", "merge-pull-request"}
	if got := Cohort(*reordered); got != multipleLabel {
		t.Fatalf("reordered side effects cohort = %q, want %q", got, multipleLabel)
	}
}

func TestCohortFilteringAndStrata(t *testing.T) {
	points := []Point{{ID: "g/a", Cohort: "review"}, {ID: "g/b", Cohort: "deploy"}, {ID: "g/c", Cohort: "review"}}
	filtered := pointsInCohort(points, "review")
	if len(filtered) != 2 || filtered[0].ID != "g/a" || filtered[1].ID != "g/c" {
		t.Fatalf("filtered = %+v", filtered)
	}
	strata := cohortStrata(points)
	if strata["g/b"] != "deploy" {
		t.Fatalf("strata = %+v", strata)
	}
	labels := cohortLabels(points)
	if len(labels) != 2 || labels[0] != "deploy" || labels[1] != "review" {
		t.Fatalf("labels = %v", labels)
	}
}
