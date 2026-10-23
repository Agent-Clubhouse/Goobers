package engine

// Production-lane fixtures for the parity harness.
//
// The parity rows that matter are the ones a PRODUCTION lane exercises, so the
// harness compiles the real reference-workflows definitions rather than a
// hand-written approximation of them. Reading the shipped YAML through the
// production config loader is what keeps a row honest: if backlog-curation's
// query-backlog stage stops running `goobers backlog-query --claim`, the
// defaulting row stops testing anything, and this loader is where that shows
// up (laneTask fails loudly on a renamed stage).

import (
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// referenceWorkflowsRoot is the repo's shipped config tree, relative to
// internal/engine. cmd/goobers' docs-updater dry-run test loads it the same
// way; this is the sanctioned path, not a test-local copy of the lanes.
func referenceWorkflowsRoot() string {
	return filepath.Join("..", "..", "reference-workflows")
}

// loadProductionLane returns one shipped workflow definition by gaggle and
// name, loaded through the production config loader so the parity fixture and
// the daemon see byte-identical specs.
func loadProductionLane(t *testing.T, gaggle, name string) apiv1.Workflow {
	t.Helper()
	set, report, err := instance.LoadConfigDir(referenceWorkflowsRoot())
	if err != nil {
		t.Fatalf("load reference-workflows: %v\n%v", err, report)
	}
	for i := range set.Workflows {
		w := set.Workflows[i]
		if w.Spec.Gaggle == gaggle && w.Name == name {
			return w
		}
	}
	t.Fatalf("reference-workflows has no workflow %q in gaggle %q", name, gaggle)
	return apiv1.Workflow{}
}

// laneTask returns a copy of one named task from a lane, failing the test when
// the lane no longer declares it. Every fixture derived from a lane goes
// through this so a renamed or deleted stage breaks the parity row instead of
// silently emptying it.
func laneTask(t *testing.T, lane apiv1.Workflow, name string) apiv1.Task {
	t.Helper()
	for _, task := range lane.Spec.Tasks {
		if task.Name == name {
			return *task.DeepCopy()
		}
	}
	t.Fatalf("lane %q no longer declares stage %q — the parity row derived from it is testing nothing", lane.Name, name)
	return apiv1.Task{}
}

// backlogCurationLane loads the first lane scheduled to move to the engine
// (finding 002 R11: least residue).
func backlogCurationLane(t *testing.T) apiv1.Workflow {
	t.Helper()
	return loadProductionLane(t, "goobers", "backlog-curation")
}

// implementationLane loads the second lane scheduled to move.
func implementationLane(t *testing.T) apiv1.Workflow {
	t.Helper()
	return loadProductionLane(t, "goobers", "implementation")
}

// laneChain rebuilds a bounded fixture from a lane's real stages: the named
// stages in order, each rewired to the next one, with the last terminal. Every
// other field of every stage — command, inputs, capabilities, policyActions,
// goober, workspace — is the lane's own.
//
// Trimming rather than walking the whole lane is deliberate for the
// single-behaviour rows: a row that pins backlog-query defaulting should fail
// for exactly that reason, not because an unrelated stage of the same lane
// also diverges. The whole-lane walk is its own row
// (rowLaneBacklogCuration).
func laneChain(t *testing.T, lane apiv1.Workflow, stages ...string) apiv1.WorkflowSpec {
	t.Helper()
	if len(stages) == 0 {
		t.Fatal("laneChain needs at least one stage")
	}
	tasks := make([]apiv1.Task, 0, len(stages))
	for i, name := range stages {
		task := laneTask(t, lane, name)
		if i+1 < len(stages) {
			task.Next = stages[i+1]
		} else {
			task.Next = ""
		}
		tasks = append(tasks, task)
	}
	return apiv1.WorkflowSpec{
		Gaggle:   lane.Spec.Gaggle,
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
		Start:    stages[0],
		Tasks:    tasks,
	}
}
