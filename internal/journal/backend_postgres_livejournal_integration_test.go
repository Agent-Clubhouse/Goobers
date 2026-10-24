//go:build cloudintegration

package journal_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
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
