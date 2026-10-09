package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func driftTestMachine(t *testing.T, goal string) *workflow.Machine {
	t.Helper()
	return driftTestWorkflowMachine(t, "implementation", goal, nil)
}

func driftTestWorkflowMachine(t *testing.T, name, goal string, controls *apiv1.RunControls) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name: name, Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers", Start: "implement", RunControls: controls,
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
	newDriftTestRunWithControls(t, l, runID, machine, nil, snapshot, terminal)
}

func newDriftTestRunWithControls(t *testing.T, l instance.Layout, runID string, machine *workflow.Machine, controls *apiv1.RunControls, snapshot bool, terminal bool) {
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
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle, RunControls: controls,
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
	drift, err := inspectWorkflowDigestDrift(l, machines, nil)
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

func driftTestGaggles(maxRepasses int32) *instance.ConfigSet {
	return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "goobers"},
		Spec:       apiv1.GaggleSpec{RunControls: &apiv1.RunControls{MaxRepasses: maxRepasses}},
	}}}
}

// TestInspectWorkflowDigestDriftReportsRunsSupersededByThisReload is #5898:
// with definition watching on, an applied edit never reaches an in-flight
// run, which keeps the workflow and goober content (and so the stage
// timeouts) it launched with. The reload must
// name exactly the runs it left behind: not runs launched on the new
// definitions, not terminal runs, and not runs an earlier reload already
// superseded when this reload changed nothing they pinned.
func TestInspectWorkflowDigestDriftReportsRunsSupersededByThisReload(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	pinned := driftTestMachine(t, "implement")
	served := driftTestMachine(t, "implement, but edited")
	newDriftTestRun(t, l, "run-recoverable", pinned, true, false)
	newDriftTestRun(t, l, "run-at-risk", pinned, false, false)
	newDriftTestRun(t, l, "run-current", served, true, false)
	newDriftTestRun(t, l, "run-terminal", pinned, false, true)

	key := localscheduler.WorkflowIdentity{Gaggle: "goobers", Workflow: "implementation"}
	defs := func(machine *workflow.Machine, gooberDigest string, maxRepasses int32) pinnedDefinitions {
		return newPinnedDefinitions(nil,
			map[localscheduler.WorkflowIdentity]*workflow.Machine{key: machine},
			map[localscheduler.WorkflowIdentity]string{key: gooberDigest}, nil, driftTestGaggles(maxRepasses))
	}
	cases := []struct {
		name          string
		before, after pinnedDefinitions
		want          []string
	}{
		{"workflow edit", defs(pinned, "", 3), defs(served, "", 3), []string{"run-at-risk", "run-recoverable"}},
		{"unrelated reload", defs(served, "", 3), defs(served, "", 3), nil},
		{"goober edit", defs(served, "", 3), defs(served, "sha256:goober-edited", 3), []string{"run-at-risk", "run-current", "run-recoverable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drift, err := inspectWorkflowDigestDrift(l, tc.after.machines, supersededWorkflows(tc.before, tc.after))
			if err != nil {
				t.Fatalf("inspectWorkflowDigestDrift: %v", err)
			}
			if fmt.Sprint(drift.Superseded) != fmt.Sprint(tc.want) {
				t.Fatalf("superseded = %v, want %v", drift.Superseded, tc.want)
			}
			if len(drift.Recoverable) != 1 || len(drift.AtRisk) != 1 {
				t.Fatalf("standing drift = %+v, want one recoverable and one at-risk run regardless of this reload", drift)
			}
		})
	}
}

// TestInspectWorkflowDigestDriftComparesEffectiveRunControls is the #5898
// maxRepasses case: a gaggle runControls edit supersedes only the in-flight
// runs whose pinned effective controls it changes. A workflow that overrides
// the edited field masks the edit and must stay silent, as must a run already
// launched under the new effective controls; a legacy run with no pinned
// controls cannot show it is current and is reported.
func TestInspectWorkflowDigestDriftComparesEffectiveRunControls(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	plain := driftTestMachine(t, "implement")
	overridden := driftTestWorkflowMachine(t, "overridden", "implement", &apiv1.RunControls{MaxRepasses: 5})
	machines := map[localscheduler.WorkflowIdentity]*workflow.Machine{
		{Gaggle: "goobers", Workflow: "implementation"}: plain,
		{Gaggle: "goobers", Workflow: "overridden"}:     overridden,
	}
	before := newPinnedDefinitions(nil, machines, nil, nil, driftTestGaggles(3))
	after := newPinnedDefinitions(nil, machines, nil, nil, driftTestGaggles(9))
	pinned := func(key localscheduler.WorkflowIdentity, defs pinnedDefinitions) *apiv1.RunControls {
		controls := defs.runControls[key].Overrides()
		return &controls
	}
	plainKey := localscheduler.WorkflowIdentity{Gaggle: "goobers", Workflow: "implementation"}
	overriddenKey := localscheduler.WorkflowIdentity{Gaggle: "goobers", Workflow: "overridden"}
	if before.runControls[overriddenKey].MaxRepasses != 5 || before.runControls[plainKey].MaxRepasses != 3 {
		t.Fatalf("fixture effective controls = %+v", before.runControls)
	}
	newDriftTestRunWithControls(t, l, "run-plain", plain, pinned(plainKey, before), true, false)
	newDriftTestRunWithControls(t, l, "run-overridden", overridden, pinned(overriddenKey, before), true, false)
	newDriftTestRunWithControls(t, l, "run-launched-after", plain, pinned(plainKey, after), true, false)
	newDriftTestRun(t, l, "run-legacy", plain, true, false)

	drift, err := inspectWorkflowDigestDrift(l, machines, supersededWorkflows(before, after))
	if err != nil {
		t.Fatalf("inspectWorkflowDigestDrift: %v", err)
	}
	if want := "[run-legacy run-plain]"; fmt.Sprint(drift.Superseded) != want {
		t.Fatalf("superseded = %v, want %s", drift.Superseded, want)
	}

	drift, err = inspectWorkflowDigestDrift(l, machines, supersededWorkflows(after, after))
	if err != nil {
		t.Fatalf("inspectWorkflowDigestDrift: %v", err)
	}
	if len(drift.Superseded) != 0 {
		t.Fatalf("unchanged run controls superseded %v, want none", drift.Superseded)
	}
}

// TestReportWorkflowDigestDriftLogsSubsequentRunsOnlyNotice keeps the #5898
// daemon-log line tied to the runs this reload superseded: silent when there
// are none (even with standing drift still journaled), and naming them when
// there are.
func TestReportWorkflowDigestDriftLogsSubsequentRunsOnlyNotice(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instanceLog.Close() })

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	if err := reportWorkflowDigestDrift(instanceLog, workflowDigestDrift{AtRisk: []string{"run-a"}}, logf); err != nil {
		t.Fatalf("reportWorkflowDigestDrift: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("standing drift alone logged %q, want silence", lines)
	}

	if err := reportWorkflowDigestDrift(instanceLog, workflowDigestDrift{
		AtRisk: []string{"run-a"}, Superseded: []string{"run-a", "run-b"},
	}, logf); err != nil {
		t.Fatalf("reportWorkflowDigestDrift: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{
		"config reload: definition change detected; will apply to subsequent runs only",
		"2 in-flight run(s)", "stage timeouts and maxRepasses", "[run-a run-b]",
	} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("notice %q missing %q", lines[0], want)
		}
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	journaled := 0
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.Runner["kind"] == journal.RunnerAnnotationWorkflowDigestDrift {
			journaled++
		}
	}
	if journaled != 2 {
		t.Fatalf("journaled %d drift annotations, want 2", journaled)
	}
}
