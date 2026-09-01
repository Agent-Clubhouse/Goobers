package localscheduler

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// contextCapturingStarter records the context its Start was given and blocks
// until released, so a test can observe that context's state at the instant a
// real Starter would still be driving the run.
type contextCapturingStarter struct {
	entered chan struct{}
	release chan struct{}
	seen    chan context.Context
}

func newContextCapturingStarter() *contextCapturingStarter {
	return &contextCapturingStarter{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		seen:    make(chan context.Context, 1),
	}
}

func (s *contextCapturingStarter) Start(ctx context.Context, _ StartRequest) (StartResult, error) {
	s.seen <- ctx
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return StartResult{Phase: "completed"}, nil
}

// dispatchContextEntry is one workflow whose Starter is the capturing one.
func dispatchContextEntry(starter Starter) WorkflowEntry {
	return WorkflowEntry{
		Gaggle:    "web",
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}
}

// TestTriggerWithDispatchContextSeparatesValidationFromRunLifetime is the
// latent bug decision 005 D1 fixes on the runner path and MUST fix before the
// engine path exists.
//
// dispatch() runs the Starter goroutine on the context it is handed. The
// trigger plane called in on request.Context(), which Go cancels the instant
// the HTTP handler returns 200. For the local runner that silently drained a
// trigger-plane-started run at its first stage boundary — visible only as a
// mysteriously abandoned run. For an engine-driven run, whose Starter BLOCKS
// on the workflow's Get, it is worse: the await returns immediately, the
// scheduler's concurrency slot is released, every terminal hook is skipped,
// and the workflow keeps executing on Temporal with nothing watching it.
//
// The separated form validates on the caller's context and dispatches on the
// daemon's. This test cancels the caller's context WHILE the starter is
// running and requires that the starter's context is unaffected.
func TestTriggerWithDispatchContextSeparatesValidationFromRunLifetime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(sched *Scheduler, ctx, dispatchCtx context.Context) (string, error)
	}{
		{
			"unqualified",
			func(s *Scheduler, ctx, dispatchCtx context.Context) (string, error) {
				return s.TriggerWithDispatchContext(ctx, dispatchCtx, "implement", time.Now())
			},
		},
		{
			"exact",
			func(s *Scheduler, ctx, dispatchCtx context.Context) (string, error) {
				return s.TriggerExactWithDispatchContext(ctx, dispatchCtx,
					WorkflowIdentity{Gaggle: "web", Workflow: "implement"}, time.Now())
			},
		},
		{
			"priority",
			func(s *Scheduler, ctx, dispatchCtx context.Context) (string, error) {
				return s.TriggerPriorityWithDispatchContext(ctx, dispatchCtx,
					WorkflowIdentity{Gaggle: "web", Workflow: "implement"}, "run-source", time.Now())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starter := newContextCapturingStarter()
			sched, _ := newTestScheduler(t, []WorkflowEntry{dispatchContextEntry(starter)})

			dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
			defer cancelDispatch()
			requestCtx, cancelRequest := context.WithCancel(context.Background())

			done := make(chan error, 1)
			go func() {
				_, err := tc.trigger(sched, requestCtx, dispatchCtx)
				done <- err
			}()

			select {
			case <-starter.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the starter was never reached")
			}

			// The HTTP handler returns; Go cancels the request context.
			cancelRequest()
			time.Sleep(50 * time.Millisecond)

			startedCtx := <-starter.seen
			if startedCtx.Err() != nil {
				t.Fatalf("the starter's context was cancelled by the CALLER hanging up (%v); an engine run would be abandoned while its workflow kept executing", startedCtx.Err())
			}

			close(starter.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("trigger: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the trigger never returned")
			}
		})
	}
}

// TestTriggerWithDispatchContextStillValidatesOnTheCallersContext is the other
// half of the split. An abandoned caller must not hold an admission decision
// open: the request context is what bounds validation, so a trigger whose
// caller has already gone away is refused before it takes a slot.
func TestTriggerWithDispatchContextStillValidatesOnTheCallersContext(t *testing.T) {
	starter := newContextCapturingStarter()
	sched, _ := newTestScheduler(t, []WorkflowEntry{dispatchContextEntry(starter)})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := sched.TriggerWithDispatchContext(cancelled, context.Background(), "implement", time.Now()); err == nil {
		t.Error("an already-abandoned caller's trigger was admitted")
	}
	if _, err := sched.TriggerExactWithDispatchContext(cancelled, context.Background(),
		WorkflowIdentity{Gaggle: "web", Workflow: "implement"}, time.Now()); err == nil {
		t.Error("an already-abandoned caller's exact trigger was admitted")
	}
}

// TestTriggerDelegatesToTheDispatchContextForm: the three original methods
// must remain exactly what they were — validation and dispatch on ONE context
// — so every existing caller keeps its behaviour and the new form is opt-in.
func TestTriggerDelegatesToTheDispatchContextForm(t *testing.T) {
	starter := newContextCapturingStarter()
	sched, _ := newTestScheduler(t, []WorkflowEntry{dispatchContextEntry(starter)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sched.Trigger(ctx, "implement", time.Now())
	}()
	select {
	case <-starter.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the starter was never reached")
	}
	startedCtx := <-starter.seen
	cancel()
	time.Sleep(50 * time.Millisecond)
	if startedCtx.Err() == nil {
		t.Error("Trigger no longer dispatches on the caller's context; existing callers' cancellation semantics changed")
	}
	close(starter.release)
	<-done
}
