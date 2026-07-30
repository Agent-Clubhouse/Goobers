// Package instancefixture builds synthetic instance definitions — gaggles,
// goobers, and workflows — at a parameterizable size.
//
// It exists because two callers need the same inventory and neither could
// previously have it. The scale harness (test/scale) built the smallest
// ConfigSet the read service would accept — no gaggles and no workflows — so it
// never measured the inventory read paths at all, and could not satisfy design
// §14.4's requirement that a 2,000-workflow page issue a bounded number of
// requests. The readservice tests build a hand-written two-workflow inventory,
// which is right for asserting field-level behavior and useless for asserting
// that cost does not grow with workflow count.
//
// Both properties need the same builder, so it lives here rather than being
// duplicated or exported from a _test.go file (which package main cannot reach).
//
// Everything here is deterministic in its inputs: the same InventorySpec yields
// byte-identical definitions, so a measurement is reproducible and a differential
// test has a stable oracle.
package instancefixture

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// InventorySpec is the shape of a synthetic definition set.
type InventorySpec struct {
	// InstanceName names the instance in the manifest.
	InstanceName string
	// Gaggles is how many gaggles to synthesize. Workflows are spread across
	// them round-robin, so a gaggle-scoped query has a real subset to select and
	// the authorization-predicate rule (§5.5) has more than one value to scope by.
	Gaggles int
	// Workflows is the total workflow count across all gaggles — the dimension
	// §14.4's "a page showing W workflows issues a request count that does not
	// grow with W" is asserted against.
	Workflows int
	// GoobersPerGaggle is how many goober definitions each gaggle declares, so
	// the goober roster read path has content.
	GoobersPerGaggle int
	// TasksPerWorkflow is how many tasks each workflow declares. Above 1 the
	// tasks form a linear chain, giving stage-scoped reads real stage names.
	TasksPerWorkflow int
	// MaxConcurrentRuns is the readiness ceiling every workflow declares. It is
	// what the active-run count is compared against on the inventory surfaces,
	// so it must be non-zero for those surfaces to be meaningful.
	MaxConcurrentRuns int32
}

// GaggleName returns the deterministic name of gaggle i.
//
// Exported because a caller that generates *runs* has to attribute them to the
// same gaggles this inventory declares. When the two disagree, every inventory
// surface reports zero active runs and every measurement of them is meaningless
// while looking perfectly healthy — the failure mode is silent, so the naming is
// shared rather than reimplemented.
func GaggleName(i int) string { return fmt.Sprintf("gaggle-%03d", i) }

// WorkflowName returns the deterministic name of workflow i.
func WorkflowName(i int) string { return fmt.Sprintf("workflow-%05d", i) }

// GooberName returns the deterministic name of goober j in gaggle i.
func GooberName(gaggle, index int) string {
	return fmt.Sprintf("goober-%03d-%02d", gaggle, index)
}

// StageName returns the deterministic name of task t in a workflow.
func StageName(t int) string {
	if t == 0 {
		return "implement"
	}
	return fmt.Sprintf("stage-%02d", t)
}

// Inventory builds the ConfigSet described by spec.
//
// Preview features are enabled on the manifest because the read service requires
// it to accept a definition set at all; that is a property of the loader, not of
// this fixture.
func Inventory(spec InventorySpec) *instance.ConfigSet {
	spec = withDefaults(spec)

	names := make([]string, 0, spec.Gaggles)
	gaggles := make([]apiv1.Gaggle, 0, spec.Gaggles)
	goobers := make([]apiv1.Goober, 0, spec.Gaggles*spec.GoobersPerGaggle)
	for g := 0; g < spec.Gaggles; g++ {
		name := GaggleName(g)
		names = append(names, name)
		gaggles = append(gaggles, gaggle(name, g))
		for j := 0; j < spec.GoobersPerGaggle; j++ {
			goobers = append(goobers, goober(name, g, j))
		}
	}

	workflows := make([]apiv1.Workflow, 0, spec.Workflows)
	for w := 0; w < spec.Workflows; w++ {
		// Round-robin across gaggles keeps the spread even, so a gaggle-scoped
		// page is a predictable fraction of the whole and a fan-out assertion
		// cannot be satisfied by an accidentally-empty scope.
		workflows = append(workflows, workflowDef(spec, w, names[w%len(names)]))
	}

	return &instance.ConfigSet{
		Manifest: &apiv1.Manifest{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				workflow.PreviewFeaturesAnnotation: "true",
			}},
			Spec: apiv1.ManifestSpec{
				Instance: apiv1.InstanceRef{Name: spec.InstanceName, Environment: apiv1.EnvironmentDev},
				Gaggles:  names,
			},
		},
		Gaggles:   gaggles,
		Goobers:   goobers,
		Workflows: workflows,
	}
}

// withDefaults fills zero fields so a partially-specified spec is still valid.
// A zero workflow or gaggle count would produce an inventory that silently
// measures nothing, which is the exact failure this package exists to end.
func withDefaults(spec InventorySpec) InventorySpec {
	if spec.InstanceName == "" {
		spec.InstanceName = "fixture-instance"
	}
	if spec.Gaggles < 1 {
		spec.Gaggles = 1
	}
	if spec.Workflows < 1 {
		spec.Workflows = 1
	}
	if spec.TasksPerWorkflow < 1 {
		spec.TasksPerWorkflow = 1
	}
	if spec.MaxConcurrentRuns < 1 {
		spec.MaxConcurrentRuns = 1
	}
	if spec.GoobersPerGaggle < 0 {
		spec.GoobersPerGaggle = 0
	}
	return spec
}

func gaggle(name string, index int) apiv1.Gaggle {
	return apiv1.Gaggle{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiv1.GaggleSpec{
			DisplayName: fmt.Sprintf("Gaggle %03d", index),
			Project: apiv1.RepoRef{
				Provider: apiv1.ProviderGitHub,
				Owner:    "fixture",
				Name:     name,
			},
			Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "fixture/" + name},
		},
	}
}

func goober(gaggleName string, gaggleIndex, index int) apiv1.Goober {
	return apiv1.Goober{
		ObjectMeta: metav1.ObjectMeta{Name: GooberName(gaggleIndex, index)},
		Spec: apiv1.GooberSpec{
			Gaggle:       gaggleName,
			Role:         "implementer",
			Instructions: "instructions.md",
			Skills:       []string{"coding", "testing"},
			Capabilities: []string{"repo:push"},
		},
	}
}

func workflowDef(spec InventorySpec, index int, gaggleName string) apiv1.Workflow {
	name := WorkflowName(index)
	tasks := make([]apiv1.Task, 0, spec.TasksPerWorkflow)
	for t := 0; t < spec.TasksPerWorkflow; t++ {
		task := apiv1.Task{
			Name: StageName(t),
			Type: apiv1.TaskDeterministic,
			Goal: fmt.Sprintf("Stage %d of %s", t, name),
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
		}
		// Chain the tasks so multi-stage reads have a real traversal rather than
		// a set of unrelated roots.
		if t+1 < spec.TasksPerWorkflow {
			task.Next = StageName(t + 1)
		}
		tasks = append(tasks, task)
	}
	return apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{"goobers.dev/purpose": "Fixture workflow"},
		},
		Spec: apiv1.WorkflowSpec{
			Gaggle:      gaggleName,
			DisplayName: fmt.Sprintf("Workflow %05d", index),
			Triggers:    []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Readiness:   apiv1.ReadinessConditions{MaxConcurrentRuns: spec.MaxConcurrentRuns},
			Start:       tasks[0].Name,
			Tasks:       tasks,
		},
	}
}
