package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestRunnerWritesBackpropOnlyForEnrolledWorkflow(t *testing.T) {
	results := map[string]stubTaskResult{
		"run-enrolled:act":   {status: apiv1.ResultSuccess},
		"run-unenrolled:act": {status: apiv1.ResultSuccess},
	}

	r, runsDir := newTestRunner(t, results, nil)
	for _, test := range []struct {
		runID   string
		enabled bool
	}{
		{runID: "run-enrolled", enabled: true},
		{runID: "run-unenrolled", enabled: false},
	} {
		result, err := r.Start(context.Background(), StartInput{
			RunID: test.runID, Machine: backpropMachine(t, test.enabled),
			Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		if err != nil || result.Phase != journal.PhaseCompleted {
			t.Fatalf("%s result = %+v, err = %v", test.runID, result, err)
		}
		path := filepath.Join(runsDir, test.runID, creditgraph.RecordFileName)
		_, statErr := os.Stat(path)
		if test.enabled && statErr != nil {
			t.Fatalf("enrolled record missing: %v", statErr)
		}
		if !test.enabled && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("unenrolled record stat = %v, want not exist", statErr)
		}
	}
}

func TestBackpropFailureDoesNotChangeSuccessfulTerminal(t *testing.T) {
	runID := "run-analysis-failure"
	runsDir := t.TempDir()
	run := createBackpropRun(t, runsDir, runID, backpropMachine(t, true))
	defer func() { _ = run.Close() }()
	finalized := false
	notified := false
	attributedAfterTerminal := false
	r := &Runner{
		cfg: Config{
			NotifyTerminal: func(string, journal.RunPhase, string) error {
				notified = true
				return nil
			},
			FinalizeTerminal: func(string, journal.RunPhase) error {
				finalized = true
				return nil
			},
		},
		attributeRun: func(runDir string, terminal *journal.Event) (bool, error) {
			if terminal != nil {
				t.Fatal("attribution received a synthetic terminal event")
			}
			if !notified || !finalized {
				t.Fatalf("attribution ran before terminal publication: notified=%v finalized=%v", notified, finalized)
			}
			reader, openErr := journal.OpenRead(runDir)
			if openErr != nil {
				t.Fatal(openErr)
			}
			events, readErr := reader.Events()
			if readErr != nil {
				t.Fatal(readErr)
			}
			attributedAfterTerminal = len(events) > 0 && events[len(events)-1].Type == journal.EventRunFinished
			return true, errors.New("analysis unavailable")
		},
	}
	result, err := r.finish(runID, run, journal.PhaseCompleted, "act", 1)
	if err != nil {
		t.Fatalf("finish returned attribution error: %v", err)
	}
	if result.Phase != journal.PhaseCompleted || !notified || !finalized || !attributedAfterTerminal {
		t.Fatalf("result = %+v, notified = %v, finalized = %v, attributed after terminal = %v", result, notified, finalized, attributedAfterTerminal)
	}
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var failureObserved, finished bool
	for _, event := range events {
		failureObserved = failureObserved || event.Type == journal.EventError &&
			event.Error != nil && event.Error.Code == "backprop_attribution_failed"
		finished = finished || event.Type == journal.EventRunFinished &&
			event.Status == string(journal.PhaseCompleted)
	}
	if !failureObserved || !finished {
		t.Fatalf("events = %+v, want independent analysis error and successful run terminal", events)
	}
}

func TestRunnerWritesBackpropForFailedAndEscalatedRuns(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    apiv1.ResultStatus
		wantPhase journal.RunPhase
	}{
		{name: "failed", status: apiv1.ResultFailure, wantPhase: journal.PhaseFailed},
		{name: "escalated", status: apiv1.ResultBlocked, wantPhase: journal.PhaseEscalated},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "run-" + test.name
			r, runsDir := newTestRunner(t, map[string]stubTaskResult{
				runID + ":act": {status: test.status},
			}, nil)
			result, err := r.Start(context.Background(), StartInput{
				RunID: runID, Machine: backpropMachine(t, true),
				Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil || result.Phase != test.wantPhase {
				t.Fatalf("result = %+v, err = %v, want phase %q", result, err, test.wantPhase)
			}
			if _, err := creditgraph.ReadRunRecord(filepath.Join(runsDir, runID)); err != nil {
				t.Fatalf("read attribution: %v", err)
			}
		})
	}
}

func TestRunnerWritesBackpropForRepassedRun(t *testing.T) {
	const runID = "run-repassed"
	r, runsDir := newTestRunner(t, nil, nil)
	machine := backpropMachine(t, true)
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	for _, event := range []journal.Event{
		{Type: journal.EventStageStarted, Stage: "act", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "act", Attempt: 1, Status: string(apiv1.ResultFailure)},
		{Type: journal.EventStageStarted, Stage: "act", Attempt: 2},
		{Type: journal.EventStageFinished, Stage: "act", Attempt: 2, Status: string(apiv1.ResultSuccess)},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.finish(runID, run, journal.PhaseCompleted, "act", 2)
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("finish = %+v, err = %v", result, err)
	}
	record, err := creditgraph.ReadRunRecord(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Attribution.Contributions) == 0 {
		t.Fatalf("repassed attribution = %+v", record.Attribution)
	}
}

func TestResumeRefusalWritesBackpropForPinnedEnrolledWorkflow(t *testing.T) {
	const runID = "run-enrolled-refusal"
	r, runsDir := newTestRunner(t, nil, nil)
	pinned := backpropMachineWithGoal(t, "pinned")
	definition, err := json.Marshal(pinned.Def)
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: pinned.Def.Name, WorkflowVersion: pinned.Def.Version,
		WorkflowDigest: pinned.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	run.SetMachineState("act")
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	attributedAfterTerminal := false
	r.attributeRun = func(runDir string, terminal *journal.Event) (bool, error) {
		if terminal != nil {
			t.Fatal("resume refusal attribution received a synthetic terminal event")
		}
		reader, openErr := journal.OpenRead(runDir)
		if openErr != nil {
			t.Fatal(openErr)
		}
		events, readErr := reader.Events()
		if readErr != nil {
			t.Fatal(readErr)
		}
		attributedAfterTerminal = len(events) > 0 && events[len(events)-1].Type == journal.EventRunFinished
		return creditgraph.WriteRunRecord(runDir, nil)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: backpropMachineWithGoal(t, "changed"),
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil || result.Phase != journal.PhaseFailed {
		t.Fatalf("Resume = %+v, err = %v", result, err)
	}
	if _, err := creditgraph.ReadRunRecord(filepath.Join(runsDir, runID)); err != nil {
		t.Fatalf("read refusal attribution: %v", err)
	}
	if !attributedAfterTerminal {
		t.Fatal("resume refusal attribution ran before run.finished was durable")
	}
}

func TestExpireRunAttributesRecoveredUnownedEnrollment(t *testing.T) {
	startedAt := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		enabled bool
	}{
		{name: "enrolled", enabled: true},
		{name: "unenrolled", enabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "expire-" + test.name
			r, runsDir := newTestRunner(t, nil, nil)
			run := createBackpropRun(t, runsDir, runID, backpropMachine(t, test.enabled),
				journal.WithClock(func() time.Time { return startedAt }))
			run.SetMachineState("act")
			if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "act", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}

			result, expired, err := r.ExpireRun(runID, startedAt.Add(2*time.Hour), startedAt, time.Hour)
			if err != nil || !expired || result.Phase != journal.PhaseAborted {
				t.Fatalf("ExpireRun = %+v, %t, %v", result, expired, err)
			}
			_, recordErr := creditgraph.ReadRunRecord(filepath.Join(runsDir, runID))
			if test.enabled && recordErr != nil {
				t.Fatalf("enrolled recovered record: %v", recordErr)
			}
			if !test.enabled && !errors.Is(recordErr, os.ErrNotExist) {
				t.Fatalf("unenrolled recovered record error = %v, want not exist", recordErr)
			}
		})
	}
}

func TestResumeBackfillsTerminalAttributionExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		wantCalls int
	}{
		{name: "enrolled", enabled: true, wantCalls: 1},
		{name: "unenrolled", enabled: false, wantCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "terminal-" + test.name
			r, runsDir := newTestRunner(t, nil, nil)
			machine := backpropMachine(t, test.enabled)
			run := createBackpropRun(t, runsDir, runID, machine)
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			calls := 0
			r.attributeRun = func(runDir string, terminal *journal.Event) (bool, error) {
				calls++
				return creditgraph.WriteRunRecord(runDir, terminal)
			}
			in := ResumeInput{
				RunID: runID, Machine: machine,
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := r.Resume(context.Background(), in)
				if err != nil || result.Phase != journal.PhaseCompleted {
					t.Fatalf("Resume attempt %d = %+v, %v", attempt+1, result, err)
				}
			}
			if calls != test.wantCalls {
				t.Fatalf("attribution calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func createBackpropRun(t *testing.T, runsDir, runID string, machine *workflow.Machine, opts ...journal.Option) *journal.Run {
	t.Helper()
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	opts = append(opts, journal.WithInputIntegrity(map[string]apiv1.Integrity{
		journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
	}))
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func backpropMachine(t *testing.T, enabled bool) *workflow.Machine {
	return backpropMachineWithGoal(t, "act", enabled)
}

func backpropMachineWithGoal(t *testing.T, goal string, enabled ...bool) *workflow.Machine {
	t.Helper()
	enrolled := true
	if len(enabled) > 0 {
		enrolled = enabled[0]
	}
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web", Backprop: &apiv1.BackpropConfig{Enabled: enrolled, Version: "v1"},
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "act",
		Tasks: []apiv1.Task{{
			Name: "act", Type: apiv1.TaskDeterministic, Goal: goal,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: "backprop", Version: 1, DSLVersion: "3.0", Spec: spec,
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}
