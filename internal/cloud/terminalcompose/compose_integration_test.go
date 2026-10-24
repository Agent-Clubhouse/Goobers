//go:build cloudintegration

package terminalcompose_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalawait"
	"github.com/goobers/goobers/internal/cloud/terminalcompose"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

var (
	workspace = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	pinRepo   = apiv1.RepoRef{Provider: "github", Owner: "acme", Name: "web"}
)

// ---- fakes ----

type workflow struct {
	id     string
	result engine.RunResult
	err    error
}

func (w workflow) GetID() string                  { return w.id }
func (w workflow) GetRunID() string               { return "t" }
func (w workflow) GetFirstExecutionRunID() string { return "t" }
func (w workflow) Get(_ context.Context, out any) error {
	if w.err != nil {
		return w.err
	}
	*(out.(*engine.RunResult)) = w.result
	return nil
}
func (w workflow) GetWithOptions(ctx context.Context, out any, _ client.WorkflowRunGetOptions) error {
	return w.Get(ctx, out)
}

type temporal map[string]workflow

func (t temporal) GetWorkflow(_ context.Context, id, _ string) client.WorkflowRun { return t[id] }

type releaser struct {
	mu       sync.Mutex
	released []string
}

func (r *releaser) ReleaseRun(_ context.Context, _ *lease.Handle, runID string, _ claimrelease.Reason) ([]localscheduler.ClaimEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, runID)
	return nil, nil
}

type recorder struct {
	mu     sync.Mutex
	events []journal.Event
}

func (r *recorder) AppendBestEffort(ev journal.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

type runs struct{}

func (runs) Bind(_ context.Context, runID string) (terminalwriter.Binding, error) {
	return terminalwriter.Binding{RunID: runID, Gaggle: "web", Generation: "g1", Project: pinRepo, Workspace: workspace, WorkItems: workspace}, nil
}

// backend is the provider surface, recording what the real Writer asks of it.
type backend struct {
	mu        sync.Mutex
	updates   []providers.UpdateWorkItemRequest
	deleted   bool
	deleteErr error
}

func (b *backend) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (b *backend) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return providers.WorkItem{State: "open"}, nil
}
func (b *backend) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, nil
}
func (b *backend) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updates = append(b.updates, req)
	return providers.WorkItem{}, nil
}
func (b *backend) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return nil
}
func (b *backend) PullLabels(context.Context, providers.RepositoryRef, string) ([]string, error) {
	return nil, nil
}
func (b *backend) LabelPull(context.Context, providers.RepositoryRef, string, []string, []string) error {
	return nil
}
func (b *backend) DeleteBranch(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return providers.DeleteBranchResult{Deleted: b.deleted}, b.deleteErr
}
func (b *backend) ReleaseWorkItemClaim(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}

type backends struct{ b *backend }

func (f backends) For(context.Context, terminalwriter.Binding, providers.RepositoryRef, string) (terminalwriter.Backend, error) {
	return f.b, nil
}

// ---- rig ----

type rig struct {
	t       *testing.T
	pool    interface{ Close() }
	backend *backend
	rel     *releaser
	log     *recorder
	comp    *terminalcompose.Composition
	streaks failurestreakpg.Store
	handle  *lease.Handle
	events  map[string][]journal.Event
	closed  []terminalawait.ClosedRun
	// hideOpen makes the open-run listing empty (visibility has aged the run out).
	hideOpen bool
	reported []error
}

func newRig(t *testing.T, tm temporal, claimed map[string][]string) *rig {
	t.Helper()
	pool := pgtest.Pool(t, "tcomp")
	controller := pgtest.ControllerPool(t, pool)
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	r := &rig{t: t, backend: &backend{deleted: true}, rel: &releaser{}, log: &recorder{}, handle: handle, events: map[string][]journal.Event{}}
	r.streaks = failurestreakpg.Store{Pool: controller, Fence: manager}
	writer, err := terminalwriter.New(runs{}, backends{r.backend}, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	comp, err := terminalcompose.New(terminalcompose.Deps{
		Handle: handle, Fence: manager, Pool: controller,
		Releaser: r.rel,
		Claimed:  func(run string) ([]string, error) { return claimed[run], nil },
		Holders:  func(context.Context) ([]string, error) { return nil, nil },
		Temporal: tm,
		Open: func(context.Context) (map[string]bool, error) {
			open := map[string]bool{}
			for id := range tm {
				if r.hideOpen {
					break
				}
				open[id] = true
			}
			return open, nil
		},
		Closed: func(context.Context, time.Time) ([]terminalawait.ClosedRun, error) { return r.closed, nil },
		Pins: func(context.Context, string) (terminalawait.RunPin, error) {
			return terminalawait.RunPin{Gaggle: "web", Workflow: "implementation", RepoRef: pinRepo}, nil
		},
		Bind:   runs{}.Bind,
		Writer: writer,
		Events: func(_ context.Context, run string) ([]journal.Event, error) { return r.events[run], nil },
		Log:    r.log,
		Report: func(err error) { r.reported = append(r.reported, err) },
		Tuning: terminalcompose.Tuning{DeliveryEvery: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.comp = comp
	return r
}

func (r *rig) run(t *testing.T) {
	t.Helper()
	if err := r.comp.Awaiters.Reattach(t.Context(), r.handle, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		time.Sleep(50 * time.Millisecond)
		if err := r.comp.Awaiters.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.comp.Awaiters.Stop(t.Context())
}

func (r *rig) deliver(t *testing.T) {
	t.Helper()
	for range 3 {
		time.Sleep(5 * time.Millisecond)
		if err := r.comp.Delivery.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func outcomes(r *rig) []map[string]any {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	var out []map[string]any
	for _, ev := range r.log.events {
		if ev.Type == journal.EventRefTouched {
			out = append(out, ev.Runner)
		}
	}
	return out
}

// ---- tests ----

// A failed run: the streak is charged in PostgreSQL, the claims are released,
// and the failure comment reaches the provider through the outbox with a
// run-derived idempotency key (the real Writer refuses an unkeyed comment).
func TestAFailedRunChargesTheStreakReleasesClaimsAndDeliversTheComment(t *testing.T) {
	tm := temporal{"run-1": {id: "run-1", err: errors.New("workflow execution error")}}
	r := newRig(t, tm, map[string][]string{"run-1": {"42"}})
	r.run(t)
	r.deliver(t)

	if !slices.Equal(r.rel.released, []string{"run-1"}) {
		t.Fatalf("released = %v, want the run's claims released once", r.rel.released)
	}
	if n, err := r.streaks.Load(t.Context(), pgtest.InstanceID, failurestreakpg.Work, workspace, "42"); err != nil || n != 1 {
		t.Fatalf("streak = %d, %v; want 1", n, err)
	}
	r.backend.mu.Lock()
	defer r.backend.mu.Unlock()
	var commented bool
	for _, u := range r.backend.updates {
		if u.Comment != "" {
			commented = true
			if u.IdempotencyKey == "" {
				t.Fatalf("a comment reached the provider unkeyed: %+v", u)
			}
		}
	}
	if !commented {
		t.Fatalf("no failure comment was delivered: %+v (reported %v)", r.backend.updates, r.reported)
	}
}

// A run that closed while no leader was awaiting it, holding no claims, still
// gets its frame, exactly once.
func TestARunClosedInTheLeaderGapIsFramedOnce(t *testing.T) {
	tm := temporal{"gap-run": {id: "gap-run", result: engine.RunResult{Status: engine.StatusCompleted}}}
	r := newRig(t, tm, nil)
	r.closed = []terminalawait.ClosedRun{{RunID: "gap-run", CloseTime: time.Now().Add(-time.Minute)}}
	r.hideOpen = true // not open, no claims: only the durable listing knows it
	r.run(t)
	if count := countOf(r.rel.released, "gap-run"); count != 1 {
		t.Fatalf("gap-run framed %d times, want once (released %v)", count, r.rel.released)
	}
}

func countOf(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// The mapping conditions of the writer, end to end through the real Writer: an
// absent branch is journaled branch-not-found, a branch an open pull request
// uses is kept.
func TestPrepareJournalsMissingAndInUseBranchesLikeTheDaemon(t *testing.T) {
	pushed := []journal.Event{
		{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Kind: "branch", ID: "goobers/x"}},
		{Type: journal.EventStageFinished, Stage: "push-branch", Status: string(apiv1.ResultSuccess)},
	}
	for name, tc := range map[string]struct {
		deleted bool
		err     error
		reason  string
	}{
		"absent": {deleted: false, reason: "branch-not-found"},
		"in use": {err: &providers.BranchInUseError{}, reason: "pull-request-open"},
	} {
		t.Run(name, func(t *testing.T) {
			tm := temporal{"run-b": {id: "run-b", result: engine.RunResult{Status: engine.StatusCompleted}}}
			r := newRig(t, tm, nil)
			r.backend.deleted, r.backend.deleteErr = tc.deleted, tc.err
			r.events["run-b"] = pushed
			r.run(t)
			var got any
			for _, o := range outcomes(r) {
				if o["operation"] == "delete" {
					got = o["reason"]
				}
			}
			if got != tc.reason {
				t.Fatalf("journaled reason = %v, want %s (outcomes %v)", got, tc.reason, outcomes(r))
			}
		})
	}
}
