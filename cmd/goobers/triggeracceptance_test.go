package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func acceptedService(t *testing.T, path string, dispatch *daemonTriggerService) *durableTriggerService {
	t.Helper()
	s, err := newDurableTriggerService(path, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.queue.Close() })
	return s
}

type acceptedRunIDStarter struct{ ids chan string }

func (s *acceptedRunIDStarter) Start(_ context.Context, request localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.ids <- request.RunID
	return localscheduler.StartResult{Phase: "completed"}, nil
}

type capacityHoldingStarter struct {
	started chan string
	release chan struct{}
}

func (s *capacityHoldingStarter) Start(ctx context.Context, request localscheduler.StartRequest) (localscheduler.StartResult, error) {
	s.started <- request.RunID
	select {
	case <-s.release:
		return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
	case <-ctx.Done():
		return localscheduler.StartResult{}, ctx.Err()
	}
}

func TestDurableTriggerPinsIdentityThroughActualScheduler(t *testing.T) {
	for _, mode := range []string{"unqualified", "exact", "priority"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.db")
			dispatch := newDaemonTriggerService()
			s := acceptedService(t, path, dispatch)
			request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
			if mode != "unqualified" {
				request.Gaggle = "own"
			}
			if mode == "priority" {
				request.SourceRun = "source"
			}
			accepted, err := s.Trigger(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, path, dispatch)
			starter := &acceptedRunIDStarter{ids: make(chan string, 1)}
			scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl", Starter: starter}}, nil)
			dispatch.AttachScheduler(scheduler)
			dispatch.AttachDispatchContext(t.Context())
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			scheduler.Wait()
			status, err := s.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: request.Actor})
			if err != nil || status.State != "dispatching" || status.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
				t.Fatalf("status = %+v, %v", status, err)
			}
			select {
			case id := <-starter.ids:
				if id != status.RunID {
					t.Fatalf("starter ID=%q status ID=%q", id, status.RunID)
				}
			default:
				t.Fatal("scheduler did not start accepted run")
			}
		})
	}
}

func TestDurableTriggerRequeuesTransientCapacityRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, path, dispatch)
	release := make(chan struct{})
	starter := &capacityHoldingStarter{started: make(chan string, 3), release: release}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow:  "impl",
		Gaggle:    "own",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
		Starter:   starter,
	}}, nil)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(t.Context())
	identity := localscheduler.WorkflowIdentity{Workflow: "impl", Gaggle: "own"}
	now := time.Now()
	for i := range 2 {
		if _, err := scheduler.TriggerExactWithOptions(t.Context(), identity, now.Add(time.Duration(i)*time.Second), localscheduler.ManualTriggerOptions{BypassCadenceBudgets: true}); err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		select {
		case <-starter.started:
		case <-time.After(time.Second):
			t.Fatalf("slot %d was not occupied", i)
		}
	}
	accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{
		Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "operator", Force: true,
	})
	if err != nil || accepted.State != "accepted" {
		t.Fatalf("acceptance = %+v, %v", accepted, err)
	}
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := s.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
	if err != nil || status.State != "accepted" || status.Reason != localscheduler.ReasonMaxParallel {
		t.Fatalf("capacity status = %+v, %v", status, err)
	}
	select {
	case id := <-starter.started:
		t.Fatalf("capacity-refused trigger started early as %q", id)
	default:
	}
	close(release)
	scheduler.Wait()
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-starter.started:
		if id != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
			t.Fatalf("accepted trigger run ID = %q, want suffix of %q", id, accepted.AcceptanceID)
		}
	default:
		t.Fatal("accepted trigger was not retried after capacity freed")
	}
	status, err = s.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: "operator"})
	if err != nil || status.State != "dispatching" || status.RunID != strings.TrimPrefix(accepted.AcceptanceID, "trigger-") {
		t.Fatalf("dispatched status = %+v, %v", status, err)
	}
}

func TestDurableTriggerAcceptsDuringStartupAndOutlivesRequest(t *testing.T) {
	for _, startup := range []time.Duration{35 * time.Second, 60 * time.Second} {
		t.Run(startup.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accepted.db")
			dispatch := newDaemonTriggerService()
			now := time.Now().UTC()
			dispatch.now = func() time.Time { return now }
			s := acceptedService(t, path, dispatch)
			request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
			ctx, cancel := context.WithCancel(t.Context())
			response, err := s.Trigger(ctx, request)
			cancel()
			if err != nil || response.AcceptanceID == "" || response.State != "accepted" {
				t.Fatalf("acceptance = %+v, %v", response, err)
			}
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(startup)
			// Reopen before attaching any scheduler, proving acceptance is not
			// merely a process-local promise made during startup.
			if err := s.queue.Close(); err != nil {
				t.Fatal(err)
			}
			s = acceptedService(t, path, dispatch)
			recorder := &dispatchContextRecorder{}
			dispatch.dispatch = recorder
			dispatch.AttachDispatchContext(t.Context())
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			retry, err := s.Trigger(t.Context(), request)
			if err != nil || !retry.Duplicate || retry.AcceptanceID != response.AcceptanceID || retry.RunID != "run-1" || retry.State != "dispatching" {
				t.Fatalf("retry = %+v, %v", retry, err)
			}
			requestCtx, runCtx, calls := recorder.snapshot()
			if calls != 1 || requestCtx.Err() != nil || runCtx.Err() != nil {
				t.Fatalf("dispatch calls=%d, contexts=%v/%v", calls, requestCtx.Err(), runCtx.Err())
			}
			if err := s.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, _, calls = recorder.snapshot()
			if calls != 1 {
				t.Fatalf("repeated sweep minted %d runs", calls)
			}
		})
	}
}

func TestDurableTriggerPreservesPodAuthorityAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accepted.db")
	dispatch := newDaemonTriggerService().withGaggleContainment(func(gaggle, run string) bool { return gaggle == "own" && run == "pod-run" })
	s := acceptedService(t, path, dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "pod:pod-run", PodScoped: true, PodRunID: "pod-run"}
	response, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Close(); err != nil {
		t.Fatal(err)
	}
	// Authority changed while down. A lost json:"-" field would bypass this
	// check and accidentally dispatch the queued pod request as an operator.
	dispatch = newDaemonTriggerService().withGaggleContainment(func(string, string) bool { return false })
	recorder := &dispatchContextRecorder{}
	dispatch.dispatch = recorder
	s = acceptedService(t, path, dispatch)
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, calls := recorder.snapshot()
	if calls != 0 {
		t.Fatal("lost pod containment dispatched a run")
	}
	r, err := s.queue.Get(t.Context(), response.AcceptanceID, request.Actor)
	if err != nil || r.State != triggerqueue.Rejected {
		t.Fatalf("result = %+v, %v", r, err)
	}
}

func TestDurableTriggerReturnsAcceptanceWhileDispatchIsBlocked(t *testing.T) {
	dispatch := newDaemonTriggerService()
	barrier := &barrierTriggerer{entered: make(chan struct{}), release: make(chan struct{})}
	dispatch.dispatch = barrier
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
	original, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Drain(t.Context()) }()
	<-barrier.entered
	retry, err := s.Trigger(t.Context(), request)
	close(barrier.release)
	if drainErr := <-done; drainErr != nil {
		t.Fatal(drainErr)
	}
	if err != nil || !retry.Duplicate || retry.AcceptanceID != original.AcceptanceID || retry.State != "dispatching" {
		t.Fatalf("blocked retry = %+v, %v", retry, err)
	}
	if barrier.mints.Load() != 1 {
		t.Fatal("retry duplicated blocked dispatch")
	}
}

func TestDurableTriggerStatusBindsActorAndPodIdentity(t *testing.T) {
	dispatch := newDaemonTriggerService().withGaggleContainment(func(string, string) bool { return true })
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "own", RequestID: "delivery", Actor: "pod:run-1", PodScoped: true, PodRunID: "run-1"}
	accepted, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	lookup := httpapi.TriggerStatusRequest{AcceptanceID: accepted.AcceptanceID, Actor: request.Actor, PodScoped: true, PodRunID: "run-1"}
	status, err := s.TriggerStatus(t.Context(), lookup)
	if err != nil || status.State != "accepted" || status.AcceptanceID != accepted.AcceptanceID || status.AcceptedAt.IsZero() {
		t.Fatalf("status = %+v, %v", status, err)
	}
	for _, altered := range []httpapi.TriggerStatusRequest{
		{AcceptanceID: lookup.AcceptanceID, Actor: "other", PodScoped: true, PodRunID: "run-1"},
		{AcceptanceID: lookup.AcceptanceID, Actor: lookup.Actor, PodScoped: true, PodRunID: "run-2"},
		{AcceptanceID: lookup.AcceptanceID, Actor: lookup.Actor},
		{AcceptanceID: "missing", Actor: lookup.Actor, PodScoped: true, PodRunID: "run-1"},
	} {
		status, err := s.TriggerStatus(t.Context(), altered)
		if err == nil || status.AcceptanceID != "" || err.Error() != "trigger acceptance not found" {
			t.Fatalf("foreign status = %+v, %v", status, err)
		}
	}
}

// TestDurableTriggerRefusesUnknownWorkflowBeforeAcceptance is #5895: a
// mistyped workflow (`goobers run list`) must be refused with the available
// workflows instead of being journaled as an accepted trigger whose dispatch
// is rejected later. The startup catalog covers the window before the
// scheduler attaches; afterwards the scheduler's reload-aware catalog wins.
func TestDurableTriggerRefusesUnknownWorkflowBeforeAcceptance(t *testing.T) {
	entries := []localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl"}, {Gaggle: "other", Workflow: "impl"}, {Gaggle: "own", Workflow: "review"}}
	for _, phase := range []string{"startup catalog", "attached scheduler"} {
		t.Run(phase, func(t *testing.T) {
			dispatch := newDaemonTriggerService()
			if phase == "startup catalog" {
				dispatch.withStartupCatalog(entries)
			} else {
				dispatch.AttachScheduler(localscheduler.New(entries, nil))
			}
			s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
			for _, refused := range []struct {
				request httpapi.TriggerRequest
				status  int
				code    string
				message string
			}{
				{httpapi.TriggerRequest{Workflow: "list"}, http.StatusNotFound, "workflow_not_found", `no workflow named "list"; available workflows: other/impl, own/impl, own/review`},
				{httpapi.TriggerRequest{Workflow: "list", Gaggle: "own"}, http.StatusNotFound, "workflow_not_found", `no workflow named "list" in gaggle "own"; available workflows: impl, review`},
				{httpapi.TriggerRequest{Workflow: "review", Gaggle: "own", SourceRun: "source"}, 0, "", ""},
				{httpapi.TriggerRequest{Workflow: "impl"}, http.StatusBadRequest, "workflow_ambiguous", `workflow "impl" is ambiguous`},
			} {
				request := refused.request
				request.RequestID, request.Actor = "delivery-"+request.Gaggle+"-"+request.Workflow, "operator"
				response, err := s.Trigger(t.Context(), request)
				if refused.status == 0 {
					if err != nil || response.State != "accepted" {
						t.Fatalf("known workflow %+v: response = %+v, err = %v", request, response, err)
					}
					continue
				}
				var intervention *httpapi.InterventionError
				if !errors.As(err, &intervention) || intervention.Status != refused.status || intervention.Code != refused.code || !strings.Contains(intervention.Message, refused.message) {
					t.Fatalf("%+v: response = %+v, err = %v; want %d %s %q", request, response, err, refused.status, refused.code, refused.message)
				}
			}
			pending, err := s.queue.Pending(t.Context(), 100)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending = %+v, %v; want only the known workflow's acceptance", pending, err)
			}
		})
	}
}

// TestDurableTriggerReplaysAcceptanceAfterCatalogShrinks proves the
// acceptance-time check never breaks idempotency: a redelivery of a trigger
// accepted before a reload removed its workflow answers the original record.
func TestDurableTriggerReplaysAcceptanceAfterCatalogShrinks(t *testing.T) {
	dispatch := newDaemonTriggerService().withStartupCatalog([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl"}})
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	request := httpapi.TriggerRequest{Workflow: "impl", RequestID: "delivery", Actor: "operator"}
	original, err := s.Trigger(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	dispatch.AttachScheduler(localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "renamed"}}, nil))
	replay, err := s.Trigger(t.Context(), request)
	if err != nil || !replay.Duplicate || replay.AcceptanceID != original.AcceptanceID {
		t.Fatalf("replay = %+v, %v; want duplicate of %s", replay, err, original.AcceptanceID)
	}
}

// A pod principal naming a gaggle with no configured workflows must not be
// answered with other gaggles' catalogs.
func TestDurableTriggerUnknownGaggleRefusalStaysPodScoped(t *testing.T) {
	entries := []localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl"}}
	dispatch := newDaemonTriggerService().withStartupCatalog(entries).withGaggleContainment(func(string, string) bool { return true })
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	for _, pod := range []bool{false, true} {
		request := httpapi.TriggerRequest{Workflow: "impl", Gaggle: "ghost", RequestID: fmt.Sprint("delivery-", pod), Actor: "operator"}
		if pod {
			request.Actor, request.PodScoped, request.PodRunID = "pod:run-1", true, "run-1"
		}
		_, err := s.Trigger(t.Context(), request)
		var intervention *httpapi.InterventionError
		if !errors.As(err, &intervention) || intervention.Code != "workflow_not_found" ||
			!strings.Contains(intervention.Message, `no gaggle named "ghost" is configured`) ||
			strings.Contains(intervention.Message, "own/impl") != !pod {
			t.Fatalf("pod=%v: err = %v; want the catalog listed only for operators", pod, err)
		}
	}
}

// TestRunUnknownWorkflowAgainstLiveDaemonRecordsNoTrigger is the #5895
// reproduction end to end: `goobers run list <root>` against a running
// daemon must fail with the available workflows and leave no accepted
// trigger behind.
func TestRunUnknownWorkflowAgainstLiveDaemonRecordsNoTrigger(t *testing.T) {
	daemon := startUpOnFreeLoopback(t, freeLoopbackAddress, func(address string) string {
		root := initDeterministicDemo(t)
		setAPIListenAddress(t, root, address)
		return root
	})
	code, stdout, stderr := runArgs(t, "run", "list", daemon.root)
	daemon.cancel()
	select {
	case <-daemon.done:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if code != 1 || strings.Contains(stdout, "accepted trigger") || !strings.Contains(stderr, `workflow_not_found: no workflow named "list"; available workflows: example/`) {
		t.Fatalf("run list: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	queue, err := triggerqueue.Open(filepath.Join(instance.NewLayout(daemon.root).SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	if pending, err := queue.Pending(t.Context(), 100); err != nil || len(pending) != 0 {
		t.Fatalf("pending triggers = %+v, %v; want none", pending, err)
	}
}
