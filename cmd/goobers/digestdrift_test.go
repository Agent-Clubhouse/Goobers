package main

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func driftTestMachine(t *testing.T, goal string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: "implementation", Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers", Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: goal,
				Run: &apiv1.DeterministicRun{Command: []string{"true"}},
			}},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func newDriftTestRun(t *testing.T, l instance.Layout, runID string, machine *workflow.Machine, snapshot bool, terminal bool) {
	t.Helper()
	var inputs map[string][]byte
	var opts []journal.Option
	if snapshot {
		definition, err := json.Marshal(machine.Def)
		if err != nil {
			t.Fatal(err)
		}
		inputs = map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}
		opts = append(opts, journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	}
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, inputs, opts...)
	if err != nil {
		t.Fatal(err)
	}
	jr.SetMachineState("implement")
	if terminal {
		if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := jr.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestInspectWorkflowDigestDriftSeparatesRecoverableFromAtRisk is the #3376
// at-risk-runs surface: after a workflow edit an operator must be able to see,
// BEFORE the next restart, which in-flight runs a restart can still resume
// from their pinned definition snapshot and which ones WF-016 would refuse and
// terminate. Runs still pinned to the served digest, and terminal runs, are
// not drift at all and must not be counted.
func TestInspectWorkflowDigestDriftSeparatesRecoverableFromAtRisk(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	pinned := driftTestMachine(t, "implement")
	served := driftTestMachine(t, "implement, but edited")
	if pinned.Digest() == served.Digest() {
		t.Fatal("fixture machines must have drifted digests")
	}

	newDriftTestRun(t, l, "run-recoverable", pinned, true, false)
	newDriftTestRun(t, l, "run-at-risk", pinned, false, false)
	newDriftTestRun(t, l, "run-current", served, true, false)
	newDriftTestRun(t, l, "run-terminal", pinned, false, true)

	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{
		{Gaggle: "goobers", Workflow: "implementation"}: served,
	}
	drift, err := inspectWorkflowDigestDrift(l, machines)
	if err != nil {
		t.Fatalf("inspectWorkflowDigestDrift: %v", err)
	}
	if len(drift.Recoverable) != 1 || drift.Recoverable[0] != "run-recoverable" {
		t.Fatalf("recoverable = %v, want [run-recoverable]", drift.Recoverable)
	}
	if len(drift.AtRisk) != 1 || drift.AtRisk[0] != "run-at-risk" {
		t.Fatalf("at-risk = %v, want [run-at-risk] — a drifted run with no reconstructable definition is what a restart destroys", drift.AtRisk)
	}
}

// TestJournalWorkflowDigestDriftStaysQuietWithoutDrift keeps the report out of
// the instance log in the common case (no in-flight run pinned to a superseded
// digest) so the annotation means something when it does appear.
func TestJournalWorkflowDigestDriftStaysQuietWithoutDrift(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })

	if err := journalWorkflowDigestDrift(instanceLog, workflowDigestDrift{}); err != nil {
		t.Fatalf("journalWorkflowDigestDrift: %v", err)
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["kind"] == journal.RunnerAnnotationWorkflowDigestDrift {
			t.Fatalf("empty drift report journaled an annotation: %+v", event)
		}
	}

	if err := journalWorkflowDigestDrift(instanceLog, workflowDigestDrift{
		Recoverable: []string{"run-a"}, AtRisk: []string{"run-b", "run-c"},
	}); err != nil {
		t.Fatalf("journalWorkflowDigestDrift: %v", err)
	}
	events, err = journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["kind"] == journal.RunnerAnnotationWorkflowDigestDrift {
			found = event
		}
	}
	if found.Runner == nil {
		t.Fatal("drift report was not journaled")
	}
	if got, _ := found.Runner["atRiskCount"].(float64); int(got) != 2 {
		t.Fatalf("atRiskCount = %v, want 2", found.Runner["atRiskCount"])
	}
	if got, _ := found.Runner["recoverableCount"].(float64); int(got) != 1 {
		t.Fatalf("recoverableCount = %v, want 1", found.Runner["recoverableCount"])
	}
}
