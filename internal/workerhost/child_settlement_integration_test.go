//go:build integration

package workerhost

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type hostSettlementProbe struct {
	started, cancelled, release chan struct{}
	calls                       atomic.Int32
}

func (p *hostSettlementProbe) Dispatch(ctx context.Context, _ dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	p.calls.Add(1)
	close(p.started)
	<-ctx.Done()
	close(p.cancelled)
	<-p.release
	return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "observed-uid", WorkspaceWritersStopped: true, SurrenderConfirmed: true, Disposed: true}, ctx.Err()
}

func TestIntegrationHostKeepsClientUntilChildCustodyCommitted(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEMPORAL_CLI")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	server, err := temporaltest.StartDevServer(ctx, t, testsuite.DevServerOptions{LogLevel: "error", Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Stop() }()
	probe := &hostSettlementProbe{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(probe.release) })
	cfg := Config{HostPort: server.FrontendHostPort(), Namespace: "default", TaskQueues: []string{"host-child-settlement"}, DrainTimeout: time.Millisecond, Deps: bootstrap.EngineDeps{Dispatcher: probe}}
	start := func() (context.CancelFunc, <-chan error) {
		h, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		hostCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- h.Run(hostCtx) }()
		return stop, done
	}
	stop, done := start()
	defer stop()
	in := engine.ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: "host-settlement", Gaggle: "g", Stage: "work", Number: 1, PodAttempt: 1, ChildExecutionDigest: journal.Digest([]byte("contract"))}, Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage, Host: "image:v1"}}, Queue: cfg.TaskQueues[0]}
	run, err := server.Client().ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: engine.ChildDispatchWorkflowID(in.Attempt), TaskQueue: in.Queue}, engine.ChildDispatchOne, in)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	select {
	case <-probe.cancelled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		t.Fatal("host closed before child custody settled", err)
	case <-time.After(50 * time.Millisecond):
	}
	release.Do(func() { close(probe.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// The first Host has closed its own client. A fresh Host resumes the workflow
	// using only the completion already durably recorded by the departing Host.
	stop, done = start()
	defer func() { stop(); <-done }()
	var result engine.ChildDispatchResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.BindingDigest != in.BindingDigest() || result.Failure != "dispatch" || result.Report.ChildPodUID != "observed-uid" || !result.Report.WorkspaceWritersStopped || !result.Report.SurrenderConfirmed || !result.Report.Disposed || probe.calls.Load() != 1 {
		t.Fatal("shutdown lost proof or repeated dispatch", result, probe.calls.Load())
	}
	completed := 0
	history := server.Client().GetWorkflowHistory(ctx, run.GetID(), run.GetRunID(), false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.EventType == enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED {
			completed++
		}
	}
	if completed != 1 {
		t.Fatal("expected one durable completion", completed)
	}
}
