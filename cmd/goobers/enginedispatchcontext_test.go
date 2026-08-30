package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	"path/filepath"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// dispatchContextRecorder captures BOTH contexts a dispatch is given, so the
// split between them can be asserted rather than assumed.
type dispatchContextRecorder struct {
	mu          sync.Mutex
	requestCtx  context.Context
	dispatchCtx context.Context
	calls       int
}

func (r *dispatchContextRecorder) record(ctx, dispatchCtx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requestCtx, r.dispatchCtx = ctx, dispatchCtx
	r.calls++
}

func (r *dispatchContextRecorder) TriggerWithDispatchContext(ctx, dispatchCtx context.Context, _ string, _ time.Time) (string, error) {
	r.record(ctx, dispatchCtx)
	return "run-1", nil
}

func (r *dispatchContextRecorder) TriggerExactWithDispatchContext(ctx, dispatchCtx context.Context, _ localscheduler.WorkflowIdentity, _ time.Time) (string, error) {
	r.record(ctx, dispatchCtx)
	return "run-1", nil
}

func (r *dispatchContextRecorder) TriggerPriorityWithDispatchContext(ctx, dispatchCtx context.Context, _ localscheduler.WorkflowIdentity, _ string, _ time.Time) (string, error) {
	r.record(ctx, dispatchCtx)
	return "run-1", nil
}

func (r *dispatchContextRecorder) snapshot() (context.Context, context.Context, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requestCtx, r.dispatchCtx, r.calls
}

// TestTriggerPlaneDispatchesOnTheDaemonsLifecycleContext is the HTTP
// request-context cancellation hazard, asserted at the plane.
//
// Go's http.Server cancels a request's context the INSTANT the handler
// returns, and the trigger plane returns 200 as soon as the run is admitted.
// Before #3876 the dispatch ran on that request context, so every
// trigger-plane-started engine run had its await cancelled milliseconds after
// admission while the workflow kept executing on Temporal — the daemon
// reported a terminal the run never reached, its terminal hooks never fired,
// and its claims leaked. A client that simply hung up produced the same thing.
//
// The fix is a split: admission is still validated against the CALLER's
// context (an abandoned caller must not hold an admission decision open), but
// the run's lifecycle hangs off the daemon's.
func TestTriggerPlaneDispatchesOnTheDaemonsLifecycleContext(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request httpapi.TriggerRequest
	}{
		{"unscoped", httpapi.TriggerRequest{Workflow: "implementation"}},
		{"exact", httpapi.TriggerRequest{Gaggle: "web", Workflow: "implementation"}},
		{"priority", httpapi.TriggerRequest{Gaggle: "web", Workflow: "implementation", SourceRun: "run-0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &dispatchContextRecorder{}
			svc := newDaemonTriggerService()
			svc.dispatch = recorder

			lifecycle, cancelLifecycle := context.WithCancel(context.Background())
			defer cancelLifecycle()
			svc.AttachDispatchContext(lifecycle)

			requestCtx, cancelRequest := context.WithCancel(context.Background())
			if _, err := svc.Trigger(requestCtx, tc.request); err != nil {
				t.Fatalf("Trigger: %v", err)
			}
			// The handler returns; http.Server cancels the request context.
			cancelRequest()

			gotRequest, gotDispatch, calls := recorder.snapshot()
			if calls != 1 {
				t.Fatalf("dispatched %d times, want 1", calls)
			}
			if gotDispatch == nil {
				t.Fatal("no dispatch context was supplied")
			}
			if gotDispatch.Err() != nil {
				t.Fatalf("the dispatch context is already cancelled (%v); the workflow would be abandoned mid-flight while it keeps executing on the far side", gotDispatch.Err())
			}
			if gotRequest == nil || gotRequest.Err() == nil {
				t.Error("admission was not validated against the caller's context; an abandoned caller would hold an admission decision open")
			}
			if gotRequest == gotDispatch {
				t.Error("admission and dispatch share one context; cancelling the HTTP request cancels the run")
			}
		})
	}
}

// TestTriggerPlaneFallsBackToTheRequestContextWhenUnattached: the plane is
// constructed before the daemon's scheduler exists, and an unattached plane
// must behave exactly as it did before #3876 rather than dispatching on a nil
// context.
func TestTriggerPlaneFallsBackToTheRequestContextWhenUnattached(t *testing.T) {
	recorder := &dispatchContextRecorder{}
	svc := newDaemonTriggerService()
	svc.dispatch = recorder

	requestCtx := context.Background()
	if _, err := svc.Trigger(requestCtx, httpapi.TriggerRequest{Workflow: "implementation"}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	gotRequest, gotDispatch, _ := recorder.snapshot()
	if gotDispatch != gotRequest {
		t.Error("an unattached plane must dispatch on the caller's context, which is the pre-#3876 behaviour")
	}
}

// TestEngineRunGuardsResolveScheduledRunWorkflowID closes the caveat
// reattachEngineRun used to name.
//
// A SCHEDULED engine run's Temporal workflow id is not its Goobers run id:
// internal/engine's RunScheduled hashes the claim workflow's id into the run
// id. Describing the run id therefore returns NotFound — and NotFound is
// treated as SETTLED, which releases the scheduler's reconciled concurrency
// slot underneath a workflow that is still executing, then invites the
// scheduler to admit a second run of the same workflow. That is the
// duplicate-driver hazard the guards exist to prevent, reached from the
// recovery path.
func TestEngineRunGuardsResolveScheduledRunWorkflowID(t *testing.T) {
	const (
		runID      = "scheduled-run-hash"
		workflowID = "goobers-web-implementation-2025-01-02T00:00:00Z-run"
	)
	fake := &fakeEngineWorkflows{
		status:      enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		workflowIDs: map[string]string{runID: workflowID},
	}
	guards := &engineRunGuards{client: fake}

	// Without the resolver the guards address the run id, which the engine
	// has never heard of.
	unresolved := guards.await(context.Background(), runID)
	if unresolved.Found {
		t.Fatal("the fake answered for the run id; the fixture does not reproduce the scheduled-run shape")
	}

	resolved := guards.withWorkflowIDResolver(func(_ context.Context, id string) (string, error) {
		wf, ok := fake.workflowIDs[id]
		if !ok {
			return "", engine.ErrRunNotOpen
		}
		return wf, nil
	})
	attachment := resolved.await(context.Background(), runID)
	if !attachment.Found {
		t.Fatalf("a scheduled engine run's open workflow was still not found; described %v", fake.described)
	}
	if !attachment.Settled {
		t.Error("the resolved attachment did not settle on a completed workflow")
	}
	var addressed bool
	for _, id := range fake.described {
		if id == workflowID {
			addressed = true
		}
	}
	if !addressed {
		t.Errorf("described %v, want the resolved workflow id %q", fake.described, workflowID)
	}
}

// TestEngineRunGuardsFallBackToTheRunIDWithoutAResolver: a DIRECT engine run's
// workflow id IS its run id, and that is the overwhelming majority of runs. A
// missing resolver — and a resolver that answers ErrRunNotOpen — must leave
// them addressed exactly as before, by the run id alone, with the inverse
// never consulted for a describe that already succeeded.
func TestEngineRunGuardsFallBackToTheRunIDWithoutAResolver(t *testing.T) {
	fake := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}
	guards := &engineRunGuards{client: fake}
	if attachment := guards.await(context.Background(), "run-direct"); !attachment.Found || !attachment.Settled {
		t.Fatalf("direct run attachment = %+v, want found and settled", attachment)
	}
	var resolverCalls int
	resolving := guards.withWorkflowIDResolver(func(context.Context, string) (string, error) {
		resolverCalls++
		return "", engine.ErrRunNotOpen
	})
	if attachment := resolving.await(context.Background(), "run-direct"); !attachment.Found || !attachment.Settled {
		t.Fatalf("direct run attachment with a resolver = %+v, want found and settled", attachment)
	}
	if resolverCalls != 0 {
		t.Errorf("the open-workflow inverse was consulted %d times for a direct run, want 0 — the describe already answered", resolverCalls)
	}
	if described, _, _ := fake.snapshot(); len(described) != 2 || described[0] != "run-direct" || described[1] != "run-direct" {
		t.Errorf("described %v, want the run id itself both times", described)
	}
}

// TestEngineStartRefusesADedupeKeyWithoutDirect is piece 7's routing rule.
//
// `goobers engine-start` now DELEGATES to the running daemon by default, so
// the daemon's own per-entry selection decides engine-vs-runner and the run
// carries the daemon's pinned identity. A --dedupe-key changes the RUN ID,
// and a run id minted by the CLI cannot be reconciled with the one the daemon
// would mint for the same unit of work: the two would be different runs doing
// identical work, which is precisely what the dedupe key exists to prevent.
// So the key is only meaningful on the --direct path, where the CLI is the
// one starting the workflow, and asking for it otherwise is refused rather
// than silently ignored.
func TestEngineStartRefusesADedupeKeyWithoutDirect(t *testing.T) {
	root := initDeterministicDemo(t)
	// A daemon holding the instance lock is what makes the delegated path the
	// live one; without it engine-start legitimately starts directly and the
	// key IS meaningful.
	release, err := acquireInstanceLock(filepath.Join(instance.NewLayout(root).SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("hold the instance lock: %v", err)
	}
	t.Cleanup(release)

	var stdout, stderr strings.Builder
	code := runEngineStart([]string{"--dedupe-key", "k", "default-implement", root}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("engine-start accepted --dedupe-key against a live daemon; the CLI would mint a run id the daemon cannot reconcile")
	}
	if !strings.Contains(stderr.String(), "--direct") {
		t.Errorf("stderr = %q, want it to name --direct as the flag that makes --dedupe-key meaningful", stderr.String())
	}
}
