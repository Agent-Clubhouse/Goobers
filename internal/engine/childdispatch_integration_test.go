//go:build integration

package engine

import (
	"context"
	"io"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type childStopDispatcher struct{ started, stopped, release chan struct{} }

func (d *childStopDispatcher) Dispatch(ctx context.Context, _ dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	close(d.started)
	<-ctx.Done()
	close(d.stopped)
	<-d.release
	return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact-terminated-uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true, Disposed: true}, nil
}

// The SDK unit test environment completes cancelled activities immediately,
// even with WaitForCancellation. A real server proves the cleanup result crosses
// the cancellation boundary; no pod or external cluster is needed by this test.
func TestIntegrationChildDispatchStopRetainsTerminationProof(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
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
	d := &childStopDispatcher{started: make(chan struct{}), stopped: make(chan struct{}), release: make(chan struct{})}
	queue := "contained-dispatch-test"
	w := worker.New(server.Client(), queue, worker.Options{})
	RegisterWith(w, &Activities{Dispatcher: d})
	if err = w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	in := ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: "child", Gaggle: "g", Stage: "work", Number: 1, PodAttempt: 3, ChildExecutionDigest: journal.Digest(nil)}, Eligible: []dispatcher.RunnerSpec{{OS: "linux", HostKind: instance.RunnerHostImage}}, Queue: queue}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: ChildDispatchWorkflowID(in.Attempt), TaskQueue: queue}, ChildDispatchOne, in)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = server.Client().SignalWorkflow(ctx, run.GetID(), run.GetRunID(), ChildDispatchStopSignal, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.stopped:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(d.release)
	var result ChildDispatchResult
	if err = run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Report.WorkspaceWritersStopped || result.Report.ChildPodUID != "exact-terminated-uid" || result.BindingDigest != in.BindingDigest() {
		t.Fatal("cleanup proof lost", result)
	}
}
