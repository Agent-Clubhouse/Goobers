package engine

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
)

func TestStaticRepeatedVisitsRealHistoryReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})
	in := repeatedVisitInput("repeated-real-history", 2)
	writer, dir := newLiveWriter(t)
	var mu sync.Mutex
	calls := map[string]int{}
	acts := &Activities{Journal: writer, Workspaces: testWorkspaces(t), Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		mu.Lock()
		defer mu.Unlock()
		stage := stageOfTaskID(env.TaskID)
		calls[stage]++
		value := fmt.Sprintf("%s-%d", stage, calls[stage])
		grade := apiv1.IntegrityTrusted
		if stage == "alpha" && calls[stage] == 2 {
			grade = apiv1.IntegrityUnapproved
		}
		if stage == "join" {
			value = fmt.Sprintf("%v/%v/%v", env.Inputs["alpha"], env.Inputs["beta"], env.Inputs["subject"])
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Integrity: grade, Outputs: map[string]any{"value": value}}, nil
	}}}
	gates := 0
	acts.Auto = automatedFunc(func(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
		gates++
		if gates == 1 {
			return "fail", nil
		}
		return "pass", nil
	})
	w := worker.New(server.Client(), "repeated-static-visits", worker.Options{})
	w.RegisterWorkflow(staticScheduler)
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: in.Run.RunID, TaskQueue: "repeated-static-visits"}, staticScheduler, in)
	if err != nil {
		t.Fatal(err)
	}
	var out staticSchedulerResult
	if err := run.Get(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "" || out.Result.Status != StatusCompleted {
		t.Fatalf("result=%+v error=%s", out.Result, out.Error)
	}
	assertRepeatedVisitJournal(t, "real-server", liveEvents(t, dir, in.Run.RunID), []string{"alpha-1/beta-1/beta-1", "alpha-2/beta-2/beta-2"})
	history := hitlFetchHistory(t, ctx, server.Client(), run.GetID(), run.GetRunID())
	replay := func(h *historypb.History) {
		r := worker.NewWorkflowReplayer()
		r.RegisterWorkflow(staticScheduler)
		if err := r.ReplayWorkflowHistory(nil, h); err != nil {
			t.Fatal(err)
		}
	}
	replay(history)
	for i, event := range history.Events {
		if event.GetWorkflowTaskStartedEventAttributes() != nil {
			replay(&historypb.History{Events: history.Events[:i+1]})
		}
	}
}
