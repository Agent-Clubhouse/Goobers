package terminalawait

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// fakeRun is a workflow that closes when release is closed (or immediately
// when it is nil), with the given result or error.
type fakeRun struct {
	id      string
	release chan struct{}
	result  engine.RunResult
	err     error
}

func (r *fakeRun) GetID() string                  { return r.id }
func (r *fakeRun) GetRunID() string               { return "temporal-run" }
func (r *fakeRun) GetFirstExecutionRunID() string { return "temporal-run" }
func (r *fakeRun) Get(ctx context.Context, valuePtr any) error {
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.err != nil {
		return r.err
	}
	*(valuePtr.(*engine.RunResult)) = r.result
	return nil
}
func (r *fakeRun) GetWithOptions(ctx context.Context, valuePtr any, _ client.WorkflowRunGetOptions) error {
	return r.Get(ctx, valuePtr)
}

type fakeTemporal struct{ runs map[string]*fakeRun }

func (f fakeTemporal) GetWorkflow(_ context.Context, id, _ string) client.WorkflowRun {
	if run, ok := f.runs[id]; ok {
		return run
	}
	return &fakeRun{id: id, err: errors.New("workflow not found")}
}

type fakeClaims struct {
	mu       sync.Mutex
	calls    []string
	reasons  []claimrelease.Reason
	held     map[string][]string
	err      error
	released chan string
}

func (c *fakeClaims) ReleaseRun(_ context.Context, _ *lease.Handle, runID string, reason claimrelease.Reason) ([]localscheduler.ClaimEntry, error) {
	c.mu.Lock()
	c.calls = append(c.calls, runID)
	c.reasons = append(c.reasons, reason)
	items := c.held[runID]
	delete(c.held, runID)
	err := c.err
	c.mu.Unlock()
	if c.released != nil {
		c.released <- runID
	}
	var entries []localscheduler.ClaimEntry
	for _, item := range items {
		entries = append(entries, localscheduler.ClaimEntry{ItemID: item, RunID: runID})
	}
	return entries, err
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

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var kinds []string
	for _, ev := range r.events {
		if kind, _ := ev.Runner["kind"].(string); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

type rig struct {
	claims   *fakeClaims
	rec      *recorder
	temporal fakeTemporal
	open     map[string]bool
	holders  []string
	identify func(runID string) (Identity, error)
	reported []error
	mu       sync.Mutex
}

func newRig() *rig {
	return &rig{
		claims: &fakeClaims{held: map[string][]string{}, released: make(chan string, 16)}, rec: &recorder{},
		temporal: fakeTemporal{runs: map[string]*fakeRun{}}, open: map[string]bool{},
		identify: func(string) (Identity, error) {
			return Identity{Gaggle: "web", Workflow: "implementation", RepoRef: apiv1.RepoRef{Owner: "acme", Name: "web"}}, nil
		},
	}
}

func (r *rig) awaiters(t *testing.T, handle *lease.Handle) *Awaiters {
	t.Helper()
	a, err := New(handle, Config{
		Temporal: r.temporal,
		Identify: func(_ context.Context, runID string) (Identity, error) { return r.identify(runID) },
		Open:     func(context.Context) (map[string]bool, error) { return r.open, nil },
		ClaimHolders: func(context.Context) ([]string, error) {
			r.claims.mu.Lock()
			defer r.claims.mu.Unlock()
			return slices.Clone(r.holders), nil
		},
		Hooks: func(h *lease.Handle, id Identity) *frame.Hooks {
			return Hooks(r.claims, h, id, r.rec, nil)
		},
		Report: func(err error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reported = append(r.reported, err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func waitReleased(t *testing.T, c *fakeClaims) string {
	t.Helper()
	return <-c.released
}

func TestACompletedRunReleasesItsClaimsAndReportsThePendingMarkerRetire(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-1"] = &fakeRun{id: "run-1", result: engine.RunResult{Status: engine.StatusCompleted}}
	r.claims.held["run-1"] = []string{"42", "43"}
	r.open["run-1"] = true
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	if got := waitReleased(t, r.claims); got != "run-1" {
		t.Fatalf("released %q", got)
	}
	if err := a.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.claims.reasons[0].Disposition != claimrelease.Completed {
		t.Fatalf("reason = %+v, want completed", r.claims.reasons[0])
	}
	if !slices.Contains(r.rec.kinds(), CodeMarkerRetirePending) {
		t.Fatalf("the provider marker retire was not reported pending: %v", r.rec.kinds())
	}
	var items []string
	for _, ev := range r.rec.events {
		if ev.Runner["kind"] == CodeMarkerRetirePending {
			items, _ = ev.Runner["items"].([]string)
			if ev.RunID != "run-1" || ev.Gaggle != "web" || ev.Runner["issue"] != MarkerRetireIssue {
				t.Fatalf("pending record not stamped or cited: %+v", ev)
			}
		}
	}
	if !slices.Equal(items, []string{"42", "43"}) {
		t.Fatalf("items = %v", items)
	}
}

func TestAFailedWorkflowFiresTheFrameWithItsErrorAndFailedReason(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-2"] = &fakeRun{id: "run-2", err: errors.New("workflow execution error")}
	r.open["run-2"] = true
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	waitReleased(t, r.claims)
	_ = a.Stop(t.Context())
	if got := r.claims.reasons[0]; got.Disposition != claimrelease.Failed || got.Code != frame.WalkFailureCode {
		t.Fatalf("reason = %+v, want failed/%s", got, frame.WalkFailureCode)
	}
	// The failed handler is not implemented in the cloud yet: it says so.
	if !slices.Contains(r.rec.kinds(), CodeHookPending) {
		t.Fatalf("a failed run recorded no pending hook: %v", r.rec.kinds())
	}
}

func TestStopCancelsAwaitersWithoutFiringHooks(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-3"] = &fakeRun{id: "run-3", release: make(chan struct{})} // never closes
	r.open["run-3"] = true
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.claims.calls) != 0 || len(r.rec.events) != 0 {
		t.Fatalf("an epoch ending fired a terminal: calls %v events %v", r.claims.calls, r.rec.events)
	}
}

func TestAClosedRunStillHoldingClaimsIsPickedUp(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-4"] = &fakeRun{id: "run-4", result: engine.RunResult{Status: engine.StatusEscalated, FinalState: "implement"}}
	r.claims.held["run-4"] = []string{"7"}
	r.holders = []string{"run-4"} // closed (not in open) but still holding claims
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	waitReleased(t, r.claims)
	_ = a.Stop(t.Context())
	if got := r.claims.reasons[0].Disposition; got != claimrelease.Escalated {
		t.Fatalf("reason = %q, want escalated", got)
	}
}

func TestAnUnidentifiableRunIsReportedAndRetriedOnTick(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-5"] = &fakeRun{id: "run-5", result: engine.RunResult{Status: engine.StatusCompleted}}
	r.open["run-5"] = true
	failing := true
	identified := make(chan struct{}, 4)
	base := r.identify
	r.identify = func(run string) (Identity, error) {
		identified <- struct{}{}
		if failing {
			return Identity{}, errors.New("acceptance unavailable")
		}
		return base(run)
	}
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	<-identified // the first attempt ran and failed
	// The goroutine untracks itself when the attempt ends; wait for that, then
	// Tick restarts it (no sleeping: yield until the set is empty).
	for {
		a.mu.Lock()
		idle := len(a.running) == 0
		a.mu.Unlock()
		if idle {
			break
		}
		runtime.Gosched()
	}
	failing = false
	if err := a.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitReleased(t, r.claims)
	_ = a.Stop(t.Context())
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reported) == 0 {
		t.Fatal("the failed identification was not reported")
	}
}

func TestAwaitersAreBoundedAndNotDuplicated(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	gate := make(chan struct{})
	for i := range 5 {
		id := fmt.Sprintf("run-%d", i)
		r.temporal.runs[id] = &fakeRun{id: id, release: gate}
		r.open[id] = true
	}
	a, err := New(handle, Config{
		Temporal: r.temporal, MaxConcurrent: 2,
		Identify: func(context.Context, string) (Identity, error) { return Identity{Gaggle: "web"}, nil },
		Open:     func(context.Context) (map[string]bool, error) { return r.open, nil },
		Hooks:    func(h *lease.Handle, id Identity) *frame.Hooks { return Hooks(r.claims, h, id, r.rec, nil) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := a.Reattach(t.Context(), handle, r.open); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	running := len(a.running)
	a.mu.Unlock()
	if running != 2 {
		t.Fatalf("running awaiters = %d, want the bound of 2 (and no duplicates across passes)", running)
	}
	_ = a.Stop(t.Context())
}

func TestFinalizeIsIdempotentAndSurfacesReleaseFailures(t *testing.T) {
	rec := &recorder{}
	claims := &fakeClaims{held: map[string][]string{"run-6": {"9"}}}
	finalize := Finalize(claims, &lease.Handle{}, rec)
	out := frame.Outcome{RunID: "run-6", Phase: journal.PhaseCompleted}
	if err := finalize(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if err := finalize(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if got := rec.kinds(); len(got) != 1 {
		t.Fatalf("pending records = %v, want exactly one (the second call released nothing)", got)
	}
	claims.err = fmt.Errorf("recovering: %w", lease.ErrStale)
	if err := finalize(t.Context(), out); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("a failed release = %v, want it returned for the frame to record", err)
	}
}

func TestReleaseReasonMirrorsTheLocalTerminalMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		out  frame.Outcome
		want claimrelease.Disposition
	}{
		"completed": {frame.Outcome{Phase: journal.PhaseCompleted}, claimrelease.Completed},
		"no work":   {frame.Outcome{Phase: journal.PhaseCompleted, Result: engine.RunResult{NoWork: true}}, claimrelease.NoWork},
		"failed":    {frame.Outcome{Phase: journal.PhaseFailed, Result: engine.RunResult{FailureCode: "CI"}}, claimrelease.Failed},
		"escalated": {frame.Outcome{Phase: journal.PhaseEscalated}, claimrelease.Escalated},
		"abort":     {frame.Outcome{Phase: journal.PhaseAborted, Result: engine.RunResult{Status: engine.StatusBlocked}}, claimrelease.Abandoned},
		"cancelled": {frame.Outcome{Phase: journal.PhaseAborted}, claimrelease.Cancelled},
		"unknown":   {frame.Outcome{Phase: journal.PhaseRunning}, claimrelease.Unknown},
	} {
		if got := ReleaseReason(tc.out).Disposition; got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestNewRefusesIncompleteConfig(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("built without a handle")
	}
	if _, err := New(&lease.Handle{}, Config{}); err == nil {
		t.Fatal("built without its collaborators")
	}
	a, _ := New(&lease.Handle{}, Config{
		Temporal: fakeTemporal{}, Identify: func(context.Context, string) (Identity, error) { return Identity{}, nil },
		Open: func(context.Context) (map[string]bool, error) { return nil, nil }, Hooks: func(*lease.Handle, Identity) *frame.Hooks { return nil },
	})
	if err := a.Reattach(t.Context(), &lease.Handle{}, nil); err == nil {
		t.Fatal("reattached with another epoch's handle")
	}
}

type byRun struct {
	records map[string]triggerqueue.Record
	err     error
}

func (b byRun) ByRun(_ context.Context, run string) (triggerqueue.Record, error) {
	if b.err != nil {
		return triggerqueue.Record{}, b.err
	}
	if r, ok := b.records[run]; ok {
		return r, nil
	}
	return triggerqueue.Record{}, sql.ErrNoRows
}

func TestIdentifyFromAcceptance(t *testing.T) {
	raw, err := (startintent.Envelope{
		Kind: startintent.Kind, Request: startintent.Request{Workflow: "implementation", Gaggle: "web"},
		Target: startintent.Target{Gaggle: "web", Workflow: "implementation", ConfigGeneration: "g1", WorkflowDigest: "w", GooberDigest: "d"},
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	identify := IdentifyFromAcceptance(byRun{records: map[string]triggerqueue.Record{
		"run-1": {Payload: raw}, "run-child": {Payload: []byte(`{"kind":"goobers.child-start/v1"}`)},
	}}, func(g string) (apiv1.RepoRef, bool) { return repo, g == "web" })
	id, err := identify(t.Context(), "run-1")
	if err != nil || id.Gaggle != "web" || id.Workflow != "implementation" || id.RepoRef != repo {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	for _, run := range []string{"run-missing", "run-child"} {
		if _, err := identify(t.Context(), run); !errors.Is(err, ErrUnidentifiable) {
			t.Errorf("%s = %v, want ErrUnidentifiable", run, err)
		}
	}
	if _, err := IdentifyFromAcceptance(byRun{err: errors.New("db down")}, nil)(t.Context(), "run-1"); err == nil || errors.Is(err, ErrUnidentifiable) {
		t.Fatalf("a database error = %v, want a retryable error", err)
	}
}

func waitIdle(a *Awaiters) {
	for {
		a.mu.Lock()
		idle := len(a.running) == 0
		a.mu.Unlock()
		if idle {
			return
		}
		runtime.Gosched()
	}
}

type memWatermark struct {
	mu       sync.Mutex
	through  time.Time
	set      bool
	advances int
}

func (w *memWatermark) Load(context.Context) (time.Time, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.through, w.set, nil
}

func (w *memWatermark) Advance(_ context.Context, _ *lease.Handle, through time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if through.After(w.through) {
		w.through, w.set = through, true
		w.advances++
	}
	return nil
}

func (w *memWatermark) position() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.through
}

func TestTheFrameFiresOncePerRunWithinAnEpoch(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-1"] = &fakeRun{id: "run-1", result: engine.RunResult{Status: engine.StatusEscalated, FinalState: "implement"}}
	r.claims.held["run-1"] = []string{"1"}
	r.open["run-1"] = true // visibility lag: still listed open after it closed
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	waitReleased(t, r.claims)
	waitIdle(a)
	first := countKind(r.rec, CodeHookPending)
	if first == 0 {
		t.Fatal("the first frame recorded no hook records")
	}
	for range 6 {
		waitIdle(a)
		if err := a.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	waitIdle(a)
	_ = a.Stop(t.Context())
	if got := len(r.claims.calls); got != 1 {
		t.Fatalf("frame fired %d times in 7 passes, want once", got)
	}
	if n := countKind(r.rec, CodeHookPending); n != first {
		t.Fatalf("hook records grew %d -> %d: the frame re-ran", first, n)
	}
}

func countKind(rec *recorder, kind string) int {
	n := 0
	for _, k := range rec.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

func TestOnlyAFailedFinalizeIsRetriedWithCappedBackoff(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.temporal.runs["run-1"] = &fakeRun{id: "run-1", result: engine.RunResult{Status: engine.StatusCompleted}}
	r.claims.held["run-1"] = []string{"1"}
	r.claims.err = errors.New("claims plane unavailable")
	r.open["run-1"] = true
	r.claims.released = nil
	var clock struct {
		sync.Mutex
		now time.Time
	}
	clock.now = time.Unix(1000, 0)
	now := func() time.Time { clock.Lock(); defer clock.Unlock(); return clock.now }
	advance := func(d time.Duration) { clock.Lock(); clock.now = clock.now.Add(d); clock.Unlock() }
	a, err := New(handle, Config{
		Temporal: r.temporal, Now: now, RetryBase: time.Second, RetryMax: 4 * time.Second,
		Identify: func(_ context.Context, id string) (Identity, error) { return r.identify(id) },
		Open:     func(context.Context) (map[string]bool, error) { return r.open, nil },
		Hooks:    func(h *lease.Handle, id Identity) *frame.Hooks { return Hooks(r.claims, h, id, r.rec, nil) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	waitIdle(a)
	calls := func() int { r.claims.mu.Lock(); defer r.claims.mu.Unlock(); return len(r.claims.calls) }
	if calls() != 1 {
		t.Fatalf("calls = %d, want the first attempt", calls())
	}
	pending := countKind(r.rec, CodeHookPending)
	tick := func() { _ = a.Tick(t.Context()); waitIdle(a) }
	tick() // inside the backoff: nothing runs
	if calls() != 1 {
		t.Fatalf("retried inside the backoff: %d calls", calls())
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		advance(want - time.Nanosecond)
		tick()
		before := calls()
		advance(time.Nanosecond)
		tick()
		if calls() != before+1 {
			t.Fatalf("after a %s backoff: calls %d -> %d, want one retry", want, before, calls())
		}
	}
	if got := countKind(r.rec, CodeHookPending); got != pending {
		t.Fatalf("a Finalize retry re-ran the other hooks: %d -> %d pending records", pending, got)
	}
	r.claims.mu.Lock()
	r.claims.err = nil
	r.claims.mu.Unlock()
	advance(10 * time.Second)
	tick()
	done := calls()
	advance(time.Hour)
	tick()
	_ = a.Stop(t.Context())
	if calls() != done {
		t.Fatalf("a finalized run was retried again: %d -> %d", done, calls())
	}
}

func TestAChildWithNoAcceptanceIsNeitherReportedNorReIdentified(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 2}
	r.open["child-1"] = true
	identified := 0
	r.identify = func(string) (Identity, error) {
		r.mu.Lock()
		identified++
		r.mu.Unlock()
		return Identity{}, fmt.Errorf("%w: child-1", ErrUnidentifiable)
	}
	a := r.awaiters(t, handle)
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		waitIdle(a)
		_ = a.Tick(t.Context())
	}
	waitIdle(a)
	_ = a.Stop(t.Context())
	r.mu.Lock()
	defer r.mu.Unlock()
	if identified != 1 || len(r.reported) != 0 {
		t.Fatalf("identified %d times, reported %v; want once and silent", identified, r.reported)
	}
}

func TestARunThatClosedInTheLeaderGapWithNoClaimsStillGetsItsFrameOnce(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 3}
	base := time.Unix(10_000, 0)
	closeAt := base.Add(-time.Minute) // closed after the old leader's watermark, before this epoch began
	mark := &memWatermark{through: base.Add(-time.Hour), set: true}
	r.temporal.runs["gap-run"] = &fakeRun{id: "gap-run", result: engine.RunResult{Status: engine.StatusCompleted}}
	// Not open, not holding claims: only the durable listing knows it.
	a, err := New(handle, Config{
		Temporal: r.temporal, Now: func() time.Time { return base }, LagBound: 30 * time.Second,
		Identify:  func(_ context.Context, id string) (Identity, error) { return r.identify(id) },
		Open:      func(context.Context) (map[string]bool, error) { return r.open, nil },
		Watermark: mark,
		Closed: func(_ context.Context, since time.Time) ([]ClosedRun, error) {
			if closeAt.After(since) {
				return []ClosedRun{{RunID: "gap-run", CloseTime: closeAt}}, nil
			}
			return nil, nil
		},
		Hooks: func(h *lease.Handle, id Identity) *frame.Hooks { return Hooks(r.claims, h, id, r.rec, nil) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reattach(t.Context(), handle, r.open); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		waitIdle(a)
		if err := a.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	waitIdle(a)
	_ = a.Stop(t.Context())
	if got := r.claims.calls; len(got) != 1 || got[0] != "gap-run" {
		t.Fatalf("frames fired for %v, want exactly gap-run once", got)
	}
	if mark.position().Before(closeAt) {
		t.Fatalf("watermark %v did not pass the framed run's close %v", mark.position(), closeAt)
	}
}

func TestTheWatermarkNeverPassesAnUnframedClosedRun(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 3}
	base := time.Unix(10_000, 0)
	mark := &memWatermark{through: base.Add(-time.Hour), set: true}
	r.temporal.runs["early"] = &fakeRun{id: "early", result: engine.RunResult{Status: engine.StatusCompleted}}
	r.temporal.runs["late"] = &fakeRun{id: "late", result: engine.RunResult{Status: engine.StatusCompleted}}
	closes := []ClosedRun{
		{RunID: "early", CloseTime: base.Add(-30 * time.Minute)},
		{RunID: "late", CloseTime: base.Add(-10 * time.Minute)},
	}
	r.identify = func(id string) (Identity, error) {
		if id == "early" {
			return Identity{}, errors.New("acceptance unavailable") // transient: stays unframed
		}
		return Identity{Gaggle: "web"}, nil
	}
	a, err := New(handle, Config{
		Temporal: r.temporal, Now: func() time.Time { return base }, Margin: time.Minute,
		Identify:  func(_ context.Context, id string) (Identity, error) { return r.identify(id) },
		Open:      func(context.Context) (map[string]bool, error) { return nil, nil },
		Watermark: mark,
		Closed:    func(context.Context, time.Time) ([]ClosedRun, error) { return closes, nil },
		Hooks:     func(h *lease.Handle, id Identity) *frame.Hooks { return Hooks(r.claims, h, id, r.rec, nil) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reattach(t.Context(), handle, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(a)
	_ = a.Tick(t.Context())
	waitIdle(a)
	_ = a.Stop(t.Context())
	if got := mark.position(); !got.Before(closes[0].CloseTime) {
		t.Fatalf("watermark %v passed the unframed run closed at %v", got, closes[0].CloseTime)
	}
}

func TestAClosedRunWhoseVisibilityLagsPastTheMarginIsStillFramed(t *testing.T) {
	r := newRig()
	handle := &lease.Handle{InstanceID: "i", Epoch: 3}
	t0 := time.Unix(100_000, 0)
	var clock struct {
		sync.Mutex
		now time.Time
	}
	clock.now = t0
	now := func() time.Time { clock.Lock(); defer clock.Unlock(); return clock.now }
	advance := func(d time.Duration) { clock.Lock(); clock.now = clock.now.Add(d); clock.Unlock() }
	mark := &memWatermark{through: t0.Add(-2 * time.Hour), set: true}
	r.temporal.runs["laggy"] = &fakeRun{id: "laggy", result: engine.RunResult{Status: engine.StatusCompleted}}
	r.temporal.runs["prompt"] = &fakeRun{id: "prompt", result: engine.RunResult{Status: engine.StatusCompleted}}
	laggyClosed, promptClosed := t0.Add(time.Minute), t0.Add(2*time.Minute)
	listings := 0
	a, err := New(handle, Config{
		Temporal: r.temporal, Now: now, Margin: 10 * time.Minute, LagBound: time.Hour, ClosedEvery: time.Minute,
		Identify:  func(_ context.Context, id string) (Identity, error) { return r.identify(id) },
		Open:      func(context.Context) (map[string]bool, error) { return nil, nil },
		Watermark: mark,
		Closed: func(_ context.Context, since time.Time) ([]ClosedRun, error) {
			listings++
			var out []ClosedRun
			if promptClosed.After(since) {
				out = append(out, ClosedRun{RunID: "prompt", CloseTime: promptClosed})
			}
			// Visibility indexes "laggy" only 25 minutes after it closed: beyond
			// the margin, within the lag bound.
			if now().After(laggyClosed.Add(25*time.Minute)) && laggyClosed.After(since) {
				out = append(out, ClosedRun{RunID: "laggy", CloseTime: laggyClosed})
			}
			return out, nil
		},
		Hooks: func(h *lease.Handle, id Identity) *frame.Hooks { return Hooks(r.claims, h, id, r.rec, nil) },
	})
	if err != nil {
		t.Fatal(err)
	}
	tick := func() {
		if err := a.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		waitIdle(a)
	}
	advance(3 * time.Minute)
	if err := a.Reattach(t.Context(), handle, nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(a)
	for range 3 { // a fast tick loop between passes
		tick()
	}
	if listings != 1 {
		t.Fatalf("closed-run discovery ran %d times in a burst of ticks, want it throttled to once", listings)
	}
	advance(15 * time.Minute)
	tick() // a quiet pass: the watermark must not run ahead to now
	advance(15 * time.Minute)
	tick() // "laggy" is visible now, more than the margin after it closed
	_ = a.Stop(t.Context())
	r.claims.mu.Lock()
	defer r.claims.mu.Unlock()
	counts := map[string]int{}
	for _, c := range r.claims.calls {
		counts[c]++
	}
	if counts["laggy"] != 1 || counts["prompt"] != 1 || len(counts) != 2 {
		t.Fatalf("frames per run = %v, want each exactly once", counts)
	}
}
