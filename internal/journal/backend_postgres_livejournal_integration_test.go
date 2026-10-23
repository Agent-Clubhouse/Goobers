//go:build cloudintegration

package journal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
)

func postgresLiveWriter(t *testing.T, config journalpg.Config, options ...livejournal.Option) (*livejournal.Writer, *journalpg.Backend, string) {
	t.Helper()
	var role string
	if err := config.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "goobers_controller" {
		t.Fatalf("current_user=%q, want goobers_controller", role)
	}
	backend := postgresBackend(t, config)
	root := t.TempDir()
	options = append(options, livejournal.WithBackend(backend))
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true }, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	return writer, backend, root
}

func assertNoLocalJournal(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("PG writer created local files: %v", entries)
	}
}

func TestPostgresLiveJournalEngineRunWithoutLocalFiles(t *testing.T) {
	config := postgresJournalConfig(t)
	writer, backend, root := postgresLiveWriter(t, config)
	runID := strings.Repeat("7", 32)
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&engine.Activities{Journal: writer})
	request := engine.GateDecisionRequest{Protocol: engine.GateDecisionProtocol, Version: engine.GateDecisionProtocolVersion, RunID: runID, RequestID: "fixture", Actor: "operator:fixture", Gate: "approval", GateOccurrence: 2, Decision: "pass"}
	emitted := 0
	accepted := false
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if err != nil || info.ActivityType.Name != engine.ActEmitJournal {
			return
		}
		emitted++
		if emitted == 2 {
			env.UpdateWorkflow(engine.GateDecisionUpdateName, engine.GateDecisionUpdateID(request), &testsuite.TestUpdateCallback{
				OnAccept: func() { accepted = true },
				OnReject: func(err error) { t.Error(err) },
				OnComplete: func(_ any, err error) {
					if err != nil {
						t.Error(err)
					}
				},
			}, request)
		}
	})
	preview := true
	env.ExecuteWorkflow(engine.Run, engine.RunInput{
		RunID: runID, Gaggle: "web", WorkflowName: "human", Version: 1, DSLVersion: "3.0",
		Spec:        apiv1.WorkflowSpec{Gaggle: "web", Start: "approval", Gates: []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{Approvers: []string{"operator:fixture"}}, Branches: map[string]string{"pass": "", "reject": "@abort"}}}},
		RepoRef:     apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repository", Branch: "main"},
		TriggerKind: string(journal.TriggerManual), PreviewFeaturesEnabled: &preview, LiveJournal: true,
		HITL: &engine.HITLPolicy{Enabled: true, WaitSeconds: 60, Actors: []string{"operator:fixture"}},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("human gate decision was not accepted")
	}
	var result engine.RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.StatusCompleted {
		t.Fatalf("result=%+v", result)
	}
	reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	want := []journal.EventType{journal.EventRunStarted, journal.EventGatePaused, journal.EventGateEvaluated, journal.EventRunFinished}
	if len(events) != len(want) {
		t.Fatalf("events=%+v", events)
	}
	for i, event := range events {
		if event.Type != want[i] || event.Seq != uint64(i+1) {
			t.Fatalf("event %d = %+v", i, event)
		}
	}
	assertNoLocalJournal(t, root)
}

func TestPostgresLiveJournalOperatorCancelPreservesCommittedStage(t *testing.T) {
	for _, cancelPoint := range []string{
		"after-executor-commit",
		"after-stage-finished-emit",
	} {
		t.Run(cancelPoint, func(t *testing.T) {
			config := postgresJournalConfig(t)
			writer, backend, root := postgresLiveWriter(t, config)
			runID := strings.Repeat(
				map[string]string{
					"after-executor-commit":     "a",
					"after-stage-finished-emit": "b",
				}[cancelPoint],
				32,
			)
			executor := &cancelFixtureExecutor{}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&engine.Activities{
				Det:        executor,
				Workspaces: cancelFixtureWorkspaces{root: t.TempDir()},
				Journal:    writer,
			})
			emits := 0
			cancelled := false
			env.SetOnActivityCompletedListener(func(
				info *activity.Info,
				_ converter.EncodedValue,
				err error,
			) {
				if err != nil || cancelled {
					return
				}
				switch info.ActivityType.Name {
				case engine.ActRunDeterministic:
					if cancelPoint == "after-executor-commit" {
						cancelled = true
						env.CancelWorkflow()
					}
				case engine.ActEmitJournal:
					emits++
					if cancelPoint == "after-stage-finished-emit" &&
						emits == 3 {
						cancelled = true
						env.CancelWorkflow()
					}
				}
			})
			preview := true
			env.ExecuteWorkflow(engine.Run, engine.RunInput{
				RunID: runID, Gaggle: "web",
				WorkflowName: "cancel-journal", Version: 1,
				DSLVersion: "3.0", Spec: cancelFixtureSpec(),
				RepoRef: apiv1.RepoRef{
					Provider: apiv1.ProviderGitHub,
					Owner:    "fixture", Name: "repository", Branch: "main",
				},
				TriggerKind:            string(journal.TriggerManual),
				PreviewFeaturesEnabled: &preview,
				LiveJournal:            true,
			})
			if err := env.GetWorkflowError(); err == nil ||
				!strings.Contains(err.Error(), "canceled") {
				t.Fatalf("workflow error = %v, want cancellation", err)
			}
			calls := executor.callCounts()
			if calls["first"] != 1 || calls["second"] != 0 {
				t.Fatalf("executor calls = %+v, want first=1 second=0", calls)
			}
			reader, err := backend.OpenRun(
				t.Context(), journal.RunRef{ID: runID})
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			var stageFinished, emitFailures, runFailed, terminals int
			for _, event := range events {
				switch event.Type {
				case journal.EventStageFinished:
					if event.Stage == "first" &&
						event.Status == string(apiv1.ResultSuccess) {
						stageFinished++
					}
				case journal.EventError:
					if event.Error != nil {
						switch event.Error.Code {
						case engine.JournalEmitFailedErrorCode:
							emitFailures++
						case "run_failed":
							if strings.Contains(event.Error.Message, "canceled") {
								runFailed++
							}
						}
					}
				case journal.EventRunFinished:
					terminals++
					if event.Status != string(journal.PhaseAborted) ||
						event.TerminalCause == nil ||
						event.TerminalCause.Classification !=
							journal.TerminalOperatorAbort ||
						event.TerminalCause.Code !=
							runsemantics.RunCanceledErrorCode {
						t.Fatalf("terminal = %+v", event)
					}
				}
			}
			if stageFinished != 1 || emitFailures != 0 ||
				runFailed != 1 || terminals != 1 {
				t.Fatalf(
					"events=%+v; stage.finished=%d emit_failed=%d "+
						"run_failed=%d terminals=%d",
					events, stageFinished, emitFailures,
					runFailed, terminals,
				)
			}
			assertNoLocalJournal(t, root)
		})
	}
}

type cancelFixtureExecutor struct {
	mu    sync.Mutex
	calls map[string]int
}

func (e *cancelFixtureExecutor) Run(
	_ context.Context,
	env apiv1.InvocationEnvelope,
	_ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	stage := env.TaskID
	if index := strings.LastIndex(stage, ":"); index >= 0 {
		stage = stage[index+1:]
	}
	e.mu.Lock()
	if e.calls == nil {
		e.calls = map[string]int{}
	}
	e.calls[stage]++
	e.mu.Unlock()
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (e *cancelFixtureExecutor) callCounts() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.calls))
	for stage, count := range e.calls {
		out[stage] = count
	}
	return out
}

var _ invoke.Deterministic = (*cancelFixtureExecutor)(nil)

type cancelFixtureWorkspace struct{ path string }

func (w cancelFixtureWorkspace) Path() string { return w.path }
func (w cancelFixtureWorkspace) Remove(context.Context) error {
	return os.RemoveAll(w.path)
}

type cancelFixtureWorkspaces struct{ root string }

func (w cancelFixtureWorkspaces) Provision(
	_ context.Context,
	_ engine.WorkspaceRequest,
) (engine.Workspace, error) {
	path, err := os.MkdirTemp(w.root, "cancel-workspace-")
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return cancelFixtureWorkspace{path: absolute}, nil
}

func cancelFixtureSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web", Start: "first",
		Tasks: []apiv1.Task{
			{
				Name: "first", Type: apiv1.TaskDeterministic,
				Goal: "first", Next: "second",
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
			},
			{
				Name: "second", Type: apiv1.TaskDeterministic,
				Goal: "second",
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
			},
		},
	}
}

func TestPostgresLiveJournalReopensAndRetainsIdentity(t *testing.T) {
	config := postgresJournalConfig(t)
	writer, backend, root := postgresLiveWriter(t, config)
	runID := strings.Repeat("8", 32)
	at := time.Now().UTC()
	request := livejournal.EmitRequest{RunID: runID, Gaggle: "web", Open: &livejournal.OpenHeader{
		Identity: journal.RunIdentity{RunID: runID, InstanceID: strings.Repeat("a", 32), Gaggle: "web", Workflow: "fixture", WorkflowVersion: 1, WorkflowDigest: "sha256:fixture", Trigger: journal.Trigger{Kind: journal.TriggerManual}},
		Graph:    []byte(`{"nodes":[]}`), Definition: []byte(`{"name":"fixture"}`),
	}, Ops: []livejournal.Op{
		{Kind: livejournal.OpAppend, Key: "start", Time: at, Event: &journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)}},
		{Kind: livejournal.OpAppend, Key: "stage", Time: at, Event: &journal.Event{Type: journal.EventStageStarted, Stage: "build", Attempt: 1}},
	}}
	if _, err := writer.Emit(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	initialReader, err := backend.OpenRunReader(t.Context(), journal.RunKey{Gaggle: "web", RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := initialReader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	reopened, backend2, root2 := postgresLiveWriter(t, config)
	response, err := reopened.Emit(t.Context(), request)
	if err != nil || response.Applied != 0 || response.Deduplicated != 2 || response.Seq != 2 {
		t.Fatalf("reopen=%+v, %v", response, err)
	}
	request.Open.Identity.InstanceID = strings.Repeat("b", 32)
	request.Ops = []livejournal.Op{{Kind: livejournal.OpAppend, Key: "foreign", Time: at, Event: &journal.Event{Type: journal.EventRunnerAnnotation}}}
	if _, err := reopened.Emit(t.Context(), request); err == nil || !strings.Contains(err.Error(), "instance identity") {
		t.Fatalf("foreign header=%v", err)
	}
	afterReader, err := backend2.OpenRunReader(t.Context(), journal.RunKey{Gaggle: "web", RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	after, err := afterReader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if after.InstanceID != identity.InstanceID || !after.StartedAt.Equal(identity.StartedAt) {
		t.Fatalf("identity changed: before=%+v after=%+v", identity, after)
	}
	request.Open.Identity.InstanceID = identity.InstanceID
	if _, err := reopened.Emit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	reader, err := backend2.OpenRun(t.Context(), journal.RunRef{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != journal.EventRunStarted || events[2].Seq != 3 {
		t.Fatalf("reopened events=%+v", events)
	}
	assertNoLocalJournal(t, root)
	assertNoLocalJournal(t, root2)
}
