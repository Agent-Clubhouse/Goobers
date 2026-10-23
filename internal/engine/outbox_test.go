package engine

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestEngineOutboxUsesProjectionSequenceAsOccurrence(t *testing.T) {
	data := []byte("report")
	digest := journal.Digest(data)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartTime(time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC))
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		rec := &runJournal{proj: JournalProjection{}}
		rec.append(ctx, journal.Event{Type: journal.EventRunStarted})
		err := rec.outbox(ctx, "build", 2, journal.AttemptInfra, []dispatcher.SurrenderedOutboxFile{
			{RelPath: "reports/result.json", Digest: digest, Size: int64(len(data))},
			{RelPath: "trace.log", Digest: journal.Digest([]byte("trace")), Size: 5},
		})
		return rec.proj, err
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := env.GetWorkflowResult(&projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Ops) != 3 {
		t.Fatalf("ops=%d, want run.started plus two outbox files", len(projection.Ops))
	}
	for _, op := range projection.Ops[1:] {
		if op.Artifact == nil || op.Artifact.OutboxOccurrence != 2 {
			t.Fatalf("outbox op=%+v, want committed first event seq 2", op)
		}
	}

	events, _, err := projectedEvents(projection)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash("artifacts/outbox/build/attempt-2/occurrence-2/reports/result.json")
	if got := filepath.ToSlash(events[1].Ref.Path); got != want ||
		events[1].Integrity != apiv1.IntegrityDerived {
		t.Fatalf("event=%+v, want path %q", events[1], want)
	}
}

func TestDispatchStageCarriesValidatedOutboxManifest(t *testing.T) {
	store := surrenderStore(t)
	data := []byte("report")
	putSurrendered(t, store, "outbox-run", "build", 1, dispatcher.SurrenderedResult{
		ContractVersion:        dispatcher.SurrenderContractV3,
		OutboxExported:         true,
		OutboxExecutionStarted: true,
		Result:                 apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		Outbox: []dispatcher.SurrenderedOutboxFile{{
			RelPath: "reports/result.json", Digest: journal.Digest(data), Size: int64(len(data)),
		}},
	})
	activities := &Activities{
		Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", SurrenderConfirmed: true}},
		Surrenders: store,
	}
	input := dispatchInput("outbox-run", "build", 1)
	input.Outbox = []string{"reports"}
	result, err := activities.DispatchStage(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outbox) != 1 || result.Outbox[0].RelPath != "reports/result.json" {
		t.Fatalf("outbox=%+v", result.Outbox)
	}
}

func TestDispatchStageRefusesUndeclaredOrReviewOutbox(t *testing.T) {
	data := []byte("report")
	manifest := []dispatcher.SurrenderedOutboxFile{{
		RelPath: "reports/result.json", Digest: journal.Digest(data), Size: int64(len(data)),
	}}
	for _, test := range []struct {
		name   string
		input  DispatchStageInput
		outbox []dispatcher.SurrenderedOutboxFile
	}{
		{name: "undeclared", input: dispatchInput("outbox-refuse", "build", 1), outbox: manifest},
		{name: "review", input: func() DispatchStageInput {
			input := dispatchInput("outbox-refuse", "build", 1)
			input.Review = true
			input.Outbox = []string{"reports"}
			return input
		}(), outbox: manifest},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "outbox-refuse", "build", 1, dispatcher.SurrenderedResult{
				ContractVersion:        dispatcher.SurrenderContractV3,
				OutboxExported:         true,
				OutboxExecutionStarted: true,
				Result:                 apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				Outbox:                 test.outbox,
			})
			activities := &Activities{
				Dispatcher: &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", SurrenderConfirmed: true}},
				Surrenders: store,
			}
			if _, err := activities.DispatchStage(t.Context(), test.input); err == nil {
				t.Fatal("invalid outbox surrender was accepted")
			}
		})
	}
}

func TestDispatchRetryJournalsOutboxBeforeStageFinished(t *testing.T) {
	data := []byte("report")
	manifest := []dispatcher.SurrenderedOutboxFile{{
		RelPath: "reports/result.json", Digest: journal.Digest(data), Size: int64(len(data)),
	}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		rec := &runJournal{}
		task := apiv1.Task{Name: "build", Type: apiv1.TaskAgentic, Outbox: []string{"reports"}}
		_, err := dispatchWithRetry(
			ctx, RunInput{}, task, rec, nil,
			func(workflow.Context, int, journal.AttemptClass) (stageActivityResult, error) {
				return stageActivityResult{
					ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
					Outbox:         manifest,
				}, nil
			},
			nil,
		)
		return rec.proj, err
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := env.GetWorkflowResult(&projection); err != nil {
		t.Fatal(err)
	}
	var outboxIndex, finishedIndex = -1, -1
	for i, op := range projection.Ops {
		if op.Artifact != nil && op.Artifact.OutboxRelPath != "" {
			outboxIndex = i
		}
		if op.Event != nil && op.Event.Type == journal.EventStageFinished {
			finishedIndex = i
		}
	}
	if outboxIndex < 0 || finishedIndex < 0 || outboxIndex >= finishedIndex {
		t.Fatalf("outboxIndex=%d finishedIndex=%d projection=%+v", outboxIndex, finishedIndex, projection)
	}
}

func TestOutboxManifestHistoryReplaysAtEveryWorkflowTaskStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{
		LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	const queue = "outbox-manifest-replay"
	worker := temporalworker.New(server.Client(), queue, temporalworker.Options{})
	worker.RegisterWorkflow(outboxReplayWorkflow)
	worker.RegisterActivityWithOptions(
		func(context.Context) (DispatchStageResult, error) {
			data := []byte("report")
			return DispatchStageResult{
				ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				Outbox: []dispatcher.SurrenderedOutboxFile{{
					RelPath: "reports/result.json", Digest: journal.Digest(data), Size: int64(len(data)),
				}},
			}, nil
		},
		activity.RegisterOptions{Name: ActDispatchStage},
	)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "outbox-manifest-replay", TaskQueue: queue,
	}, outboxReplayWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := run.Get(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	history := readOutboxHistory(t, ctx, server.Client(), run)
	replayOutboxHistory(t, history)
	prefixes := 0
	for i, event := range history.Events {
		if event.EventType != enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			continue
		}
		prefixes++
		replayOutboxHistory(t, &historypb.History{
			Events: append([]*historypb.HistoryEvent(nil), history.Events[:i+1]...),
		})
	}
	if prefixes < 2 || len(projection.Ops) != 1 || projection.Ops[0].Artifact == nil {
		t.Fatalf("prefixes=%d projection=%+v", prefixes, projection)
	}
}

func outboxReplayWorkflow(ctx workflow.Context) (JournalProjection, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	var result DispatchStageResult
	if err := workflow.ExecuteActivity(ctx, ActDispatchStage).Get(ctx, &result); err != nil {
		return JournalProjection{}, err
	}
	rec := &runJournal{}
	if err := rec.outbox(ctx, "build", 1, "", result.Outbox); err != nil {
		return JournalProjection{}, err
	}
	return rec.proj, nil
}

func readOutboxHistory(t *testing.T, ctx context.Context, c client.Client, run client.WorkflowRun) *historypb.History {
	t.Helper()
	iter := c.GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	return history
}

func replayOutboxHistory(t *testing.T, history *historypb.History) {
	t.Helper()
	replayer := temporalworker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(outboxReplayWorkflow)
	if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
		t.Fatal(err)
	}
}

func TestConformanceOutboxMatchesRunnerPathsAndEvents(t *testing.T) {
	files := []journal.OutboxFile{
		{RelPath: "reports/result.json", Data: []byte("report")},
		{RelPath: "trace.log", Data: []byte("trace")},
	}
	identity := journal.RunIdentity{
		RunID: "outbox-parity", Workflow: "outbox", WorkflowVersion: 1,
		WorkflowDigest: journal.Digest([]byte("definition")), Gaggle: "web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}
	run, err := journal.Create(t.TempDir(), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.ExportOutbox("build", 1, journal.AttemptPolicy, files); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	local, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	local = local[1:]

	manifest := make([]dispatcher.SurrenderedOutboxFile, len(files))
	for i, file := range files {
		manifest[i] = dispatcher.SurrenderedOutboxFile{
			RelPath: file.RelPath, Digest: journal.Digest(file.Data), Size: int64(len(file.Data)),
		}
	}
	rec := &runJournal{}
	rec.appendAt(time.Time{}, journal.Event{Type: journal.EventRunStarted})
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		return rec.outbox(ctx, "build", 1, journal.AttemptPolicy, manifest)
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	engineEvents, _, err := projectedEvents(rec.proj)
	if err != nil {
		t.Fatal(err)
	}
	engineEvents = engineEvents[1:]
	if err := diffConformanceViews(local, engineEvents); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxExportFailureIgnoresContinueOnError(t *testing.T) {
	task := apiv1.Task{Name: "build", ContinueOnError: true, Next: "after"}
	result := runner.OutboxExportFailureResult(
		apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		context.DeadlineExceeded,
	)
	_, out, terminal := taskOutcome(
		nil, nil, task, result,
		map[string]apiv1.ResultEnvelope{"build": result}, nil, 1, nil,
	)
	if !terminal || out.Status != StatusFailed || out.FailureCode != runner.OutboxExportFailureCode {
		t.Fatalf("terminal=%v result=%+v", terminal, out)
	}
}
