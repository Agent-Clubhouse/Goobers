//go:build integration

package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type stoppedChildDispatchProbe struct {
	started chan struct{}
	calls   atomic.Int32
}

func (p *stoppedChildDispatchProbe) Dispatch(ctx context.Context, _ dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	p.calls.Add(1)
	close(p.started)
	<-ctx.Done()
	// This seam supplies physical observations; the real Kubernetes journey
	// separately establishes that the dispatcher actually makes them.
	return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "observed-original-uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true, Disposed: true}, ctx.Err()
}

type rejectedChildCompletion struct{ calls atomic.Int32 }

func (c *rejectedChildCompletion) CompleteActivityWithOptions(context.Context, client.CompleteActivityOptions) error {
	c.calls.Add(1)
	return errors.New("completion denied")
}

func TestIntegrationChildDispatchCommitsCustodyAfterWorkerStop(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, server := cancellationDevServer(t)
	for _, rejected := range []bool{false, true} {
		name := "accepted"
		if rejected {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			queue := "child-worker-stop-" + name
			probe := &stoppedChildDispatchProbe{started: make(chan struct{})}
			denied := &rejectedChildCompletion{}
			var completion ChildDispatchCompletion = server.Client()
			if rejected {
				completion = denied
			}
			start := func() temporalworker.Worker {
				w := temporalworker.New(server.Client(), queue, temporalworker.Options{WorkerStopTimeout: time.Millisecond})
				RegisterWith(w, &Activities{Dispatcher: probe, ChildDispatchCompletion: completion})
				if err := w.Start(); err != nil {
					t.Fatal(err)
				}
				return w
			}
			worker := start()
			t.Cleanup(func() {
				if worker != nil {
					worker.Stop()
				}
			})
			in := ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: name, Gaggle: "g", Stage: "work", Number: 1, PodAttempt: 4, ChildExecutionDigest: journal.Digest([]byte("contract"))}, Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage, Host: "image:v1"}}, Queue: queue}
			run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: ChildDispatchWorkflowID(in.Attempt), TaskQueue: queue}, ChildDispatchOne, in)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-probe.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			worker.Stop()
			worker = start()
			var result ChildDispatchResult
			err = run.Get(ctx, &result)
			if rejected {
				if err == nil || denied.calls.Load() != 1 || result.Report.WorkspaceWritersStopped {
					t.Fatal("uncommitted custody became a successful workflow result", result, err)
				}
			} else if err != nil || result.BindingDigest != in.BindingDigest() || result.Report.ChildPodUID != "observed-original-uid" || !result.Report.WorkspaceWritersStopped || !result.Report.SurrenderConfirmed || !result.Report.Disposed || result.Failure != "dispatch" {
				t.Fatal("stopped worker lost or changed its final report", result, err)
			}
			completed := 0
			for _, event := range cancellationHistory(t, ctx, server.Client(), run).Events {
				if event.EventType == enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED {
					completed++
				}
			}
			wantCompleted := 1
			if rejected {
				wantCompleted = 0
			}
			if completed != wantCompleted || probe.calls.Load() != 1 {
				t.Fatal("worker stop lost completion or dispatched a replacement", completed, probe.calls.Load())
			}
		})
	}
}
