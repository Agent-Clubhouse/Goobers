//go:build cloudintegration

package terminalcompose_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/terminalawait"
	"github.com/goobers/goobers/internal/cloud/terminalcompose"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
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

// Append is the fenced append the instance-level writer journals through.
func (r *recorder) Append(ev journal.Event) error {
	r.AppendBestEffort(ev)
	return nil
}

func (r *recorder) AppendBestEffort(ev journal.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

type runs struct{}

func (runs) Bind(_ context.Context, runID string) (terminalwriter.Binding, error) {
	if runID == "gone-run" {
		return terminalwriter.Binding{}, providerwriter.ErrRepositoryNotPinned // its pin has been swept
	}
	return terminalwriter.Binding{RunID: runID, Gaggle: "web", Generation: "g1", Project: pinRepo, Workspace: workspace, WorkItems: workspace}, nil
}

// backend is the provider surface, recording what the real Writer asks of it.
type backend struct {
	mu        sync.Mutex
	updates   []providers.UpdateWorkItemRequest
	deleted   bool
	deleteErr error
	// updateErr fails UpdateWorkItem (consumed one call at a time); attempts counts
	// every call, successful or not.
	updateErrs []error
	attempts   int
	// states is each work item's provider state (default "open"); getErr fails a
	// read of that item; reads counts reads per item.
	states map[string]string
	getErr map[string]error
	reads  map[string]int
}

func (b *backend) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (b *backend) GetWorkItem(_ context.Context, _ providers.RepositoryRef, id string) (providers.WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reads == nil {
		b.reads = map[string]int{}
	}
	b.reads[id]++
	if err := b.getErr[id]; err != nil {
		return providers.WorkItem{}, err
	}
	state := "open"
	if s, ok := b.states[id]; ok {
		state = s
	}
	return providers.WorkItem{ID: id, State: state}, nil
}
func (b *backend) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, nil
}
func (b *backend) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if len(b.updateErrs) > 0 {
		err := b.updateErrs[0]
		b.updateErrs = b.updateErrs[1:]
		return providers.WorkItem{}, err
	}
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
	pool    *pgxpool.Pool
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
	tuning   terminalcompose.Tuning
	// noBudget makes the shared quota ledger refuse provider reads.
	noBudget   atomic.Bool
	manager    *lease.Manager
	controller *pgxpool.Pool
	reported   []error
}

func newRig(t *testing.T, tm temporal, claimed map[string][]string) *rig {
	t.Helper()
	return newRigTuned(t, tm, claimed, terminalcompose.Tuning{DeliveryEvery: time.Millisecond})
}

func newRigTuned(t *testing.T, tm temporal, claimed map[string][]string, tuning terminalcompose.Tuning) *rig {
	t.Helper()
	return newRigFull(t, tm, claimed, tuning, false)
}

// escalatorFor returns the instance-level writer as the composition's escalator,
// or a true nil interface (never a nil pointer inside one) to compose without it.
func escalatorFor(w *terminalwriter.InstanceWriter, without bool) parking.Poster {
	if without {
		return nil
	}
	return w
}

func newRigFull(t *testing.T, tm temporal, claimed map[string][]string, tuning terminalcompose.Tuning, noEscalator bool) *rig {
	t.Helper()
	pool := pgtest.Pool(t, "tcomp")
	controller := pgtest.ControllerPool(t, pool)
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	r := &rig{t: t, backend: &backend{deleted: true}, rel: &releaser{}, log: &recorder{}, handle: handle, events: map[string][]journal.Event{}, manager: manager, controller: controller, pool: pool}
	r.streaks = failurestreakpg.Store{Pool: controller, Fence: manager}
	writer, err := terminalwriter.New(runs{}, backends{r.backend}, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	gaggle := apiv1.Gaggle{}
	gaggle.Name = "web"
	gaggle.Spec.Project = pinRepo
	escalator, err := terminalwriter.NewInstanceWriter(func() (*instance.ConfigSet, string, bool) {
		return &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}}, "g1", true
	}, backends{r.backend}, journal.NewRegistryScrubber(), r.log.Append)
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
		Bind:      runs{}.Bind,
		Writer:    writer,
		Events:    func(_ context.Context, run string) ([]journal.Event, error) { return r.events[run], nil },
		Log:       r.log,
		Report:    func(err error) { r.reported = append(r.reported, err) },
		Backends:  backends{r.backend},
		Escalator: escalatorFor(escalator, noEscalator),
		Reserve:   func(providers.ProviderKind) bool { return !r.noBudget.Load() },
		Instance: func(repo providers.RepositoryRef) (terminalwriter.Binding, bool) {
			return terminalwriter.Binding{Gaggle: "web", Generation: "g1", Project: pinRepo, Workspace: workspace, WorkItems: workspace}, repo == workspace
		},
		Tuning: tuning,
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

func updates(r *rig) []providers.UpdateWorkItemRequest {
	r.backend.mu.Lock()
	defer r.backend.mu.Unlock()
	return slices.Clone(r.backend.updates)
}

func blockedRecords(t *testing.T, r *rig) int {
	t.Helper()
	var n int
	if err := r.controller.QueryRow(t.Context(), `SELECT count(*) FROM control.blocked_record`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The parking frame, end to end: a run whose final stage reports blocked on
// another item records the learned block in PostgreSQL, parks the item as
// blocked-on-sibling, and comments on it with a keyed escalation.
func TestABlockedRunIsRecordedAndParkedThroughTheFrame(t *testing.T) {
	blocked := engine.RunResult{
		Status: engine.StatusEscalated, FinalState: "implement",
		Outputs: map[string]apiv1.ResultEnvelope{"implement": {Status: apiv1.ResultBlocked, Summary: "waits on 41", Outputs: map[string]any{"blockedBy": "41"}}},
	}
	r := newRig(t, temporal{"run-p": {id: "run-p", result: blocked}}, map[string][]string{"run-p": {"42"}})
	r.run(t)
	if n := blockedRecords(t, r); n != 1 {
		t.Fatalf("blocked records = %d, want the learned block recorded", n)
	}
	var parked, escalated bool
	for _, u := range updates(r) {
		parked = parked || (u.ID == "42" && slices.Contains(u.AddLabels, providers.LabelBlockedOnSibling) && slices.Contains(u.RemoveLabels, providers.LabelReady))
		escalated = escalated || (u.Comment != "" && u.IdempotencyKey != "")
	}
	if !parked || !escalated {
		t.Fatalf("parked %v, keyed escalation comment %v; updates %+v (reported %v)", parked, escalated, updates(r), r.reported)
	}
}

// The notify frame, end to end: an escalated run that is not a blocked stage
// charges the item's failure streak through the circuit breaker.
func TestAnEscalatedRunChargesTheStreakThroughTheNotifyFrame(t *testing.T) {
	escalated := engine.RunResult{Status: engine.StatusEscalated, FinalState: "ci"}
	r := newRig(t, temporal{"run-n": {id: "run-n", result: escalated}}, map[string][]string{"run-n": {"42"}})
	r.run(t)
	if n, err := r.streaks.Load(t.Context(), pgtest.InstanceID, failurestreakpg.Work, workspace, "42"); err != nil || n != 1 {
		t.Fatalf("streak = %d, %v; want the escalated terminal charged once (reported %v)", n, err, r.reported)
	}
}

// A report the provider deferred stays owed, and the delivery loop sends it once
// the provider recovers, within its budget.
func TestADeferredReportIsDeliveredByATick(t *testing.T) {
	failed := temporal{"run-d": {id: "run-d", err: errors.New("workflow execution error")}}
	r := newRigTuned(t, failed, map[string][]string{"run-d": {"42"}}, terminalcompose.Tuning{DeliveryEvery: time.Millisecond, DeliveryBudget: 1})
	reset := time.Now().Add(400 * time.Millisecond)
	r.backend.updateErrs = []error{&providers.RateLimitError{Provider: providers.ProviderGitHub, Reset: reset}}
	r.run(t)
	if len(updates(r)) != 0 {
		t.Fatalf("the report was delivered while the provider was limiting: %+v", updates(r))
	}
	time.Sleep(time.Until(reset) + 3500*time.Millisecond) // the retry delay is the reset plus 2-3s of jitter
	r.deliver(t)
	var commented bool
	for _, u := range updates(r) {
		commented = commented || (u.Comment != "" && u.IdempotencyKey != "")
	}
	if !commented {
		t.Fatalf("the deferred report was not delivered by the loop (attempts %d, reported %v)", r.backend.attempts, r.reported)
	}
	if st := r.comp.Delivery.Status(); st.Pending != 0 || !st.Healthy {
		t.Fatalf("status after delivery = %+v, want nothing owed and healthy", st)
	}
}

// A report older than the bound makes the controller report itself unhealthy.
func TestALaggingOutboxReportsUnhealthy(t *testing.T) {
	failed := temporal{"run-l": {id: "run-l", err: errors.New("workflow execution error")}}
	r := newRigTuned(t, failed, map[string][]string{"run-l": {"42"}}, terminalcompose.Tuning{DeliveryEvery: time.Millisecond, MaxOutboxLag: 50 * time.Millisecond})
	r.backend.updateErrs = []error{&providers.RateLimitError{Provider: providers.ProviderGitHub, Reset: time.Now().Add(time.Hour)}}
	r.run(t)
	time.Sleep(200 * time.Millisecond)
	r.deliver(t)
	st := r.comp.Delivery.Status()
	if st.Pending == 0 || st.Healthy {
		t.Fatalf("status = %+v, want the owed report counted and unhealthy", st)
	}
}

// A deposed leader's delivery pass ends its epoch instead of writing.
func TestAStaleFenceEndsTheDeliveryLoopsEpoch(t *testing.T) {
	r := newRig(t, temporal{}, nil)
	pgtest.Takeover(t, r.pool, pgtest.ControllerManager(t, r.pool, "successor", 0))
	var ended bool
	for range 5 {
		time.Sleep(5 * time.Millisecond)
		if err := r.comp.Delivery.Tick(t.Context()); err != nil {
			ended = epoch.IsStale(err)
			break
		}
	}
	if !ended {
		t.Fatal("a delivery pass under a stale fence did not end the epoch")
	}
}

// The self-heal verifies a recorded block's items through the applied
// configuration, so it works after the recording run's pin is swept; a closed
// item is forgotten, an unverifiable one is kept, and not re-read every pass.
func TestHealForgetsAClosedBlockerAndKeepsAnUnverifiableOne(t *testing.T) {
	r := newRigTuned(t, temporal{}, nil, terminalcompose.Tuning{DeliveryEvery: time.Millisecond, HealEvery: time.Millisecond, HealLimit: 50})
	record := func(item, blocker string) {
		store := blockedrecordspg.Store{Fence: r.manager}
		if _, err := store.RecordBlock(t.Context(), r.handle, parking.Block{Repository: workspace, ItemID: item, Blockers: []string{blocker}, RunID: "gone-run", Stage: "implement"}); err != nil {
			t.Fatal(err)
		}
	}
	record("1", "2") // 2 closed since
	record("3", "4") // 4 cannot be verified
	r.backend.states = map[string]string{"2": "closed"}
	r.backend.getErr = map[string]error{"4": errors.New("provider unavailable")}
	if n := blockedRecords(t, r); n != 2 {
		t.Fatalf("seeded records = %d, want 2", n)
	}
	r.deliver(t)
	r.deliver(t)
	if n := blockedRecords(t, r); n != 1 {
		t.Fatalf("records after heal = %d, want only the unverifiable block kept (reported %v)", n, r.reported)
	}
	r.backend.mu.Lock()
	reads := r.backend.reads["4"]
	r.backend.mu.Unlock()
	if reads != 1 {
		t.Fatalf("the unverifiable item was read %d times across passes, want once (then backed off)", reads)
	}
}

// The deferral end to end: a block that closes a cycle while the provider read
// budget is spent parks its members without escalating; the delivery loop's sweep
// later re-verifies the cycle and escalates it through the instance-level writer
// (no run involved, keyed comments, every write journaled), exactly once.
func TestADeferredCycleIsEscalatedByTheSweepThroughTheInstanceWriter(t *testing.T) {
	blockedOn := func(id, on string) engine.RunResult {
		return engine.RunResult{
			Status: engine.StatusEscalated, FinalState: "implement",
			Outputs: map[string]apiv1.ResultEnvelope{"implement": {Status: apiv1.ResultBlocked, Summary: "waits", Outputs: map[string]any{"blockedBy": on}}},
		}
	}
	tm := temporal{"run-a": {id: "run-a", result: blockedOn("42", "43")}, "run-b": {id: "run-b", result: blockedOn("43", "42")}}
	r := newRigTuned(t, tm, map[string][]string{"run-a": {"42"}, "run-b": {"43"}},
		terminalcompose.Tuning{DeliveryEvery: time.Millisecond, HealEvery: time.Millisecond, DeferralBound: time.Hour})
	r.noBudget.Store(true) // the second block cannot be verified
	r.run(t)
	if n := blockedRecords(t, r); n != 2 {
		t.Fatalf("blocked records = %d, want both blocks recorded", n)
	}
	for _, u := range updates(r) {
		if slices.Contains(u.AddLabels, providers.LabelNeedsHuman) {
			t.Fatalf("a cycle that could not be verified was escalated already: %+v", u)
		}
	}
	var deferred int
	if err := r.controller.QueryRow(t.Context(), `SELECT count(*) FROM control.blocked_record WHERE verify_after IS NOT NULL`).Scan(&deferred); err != nil || deferred != 1 {
		t.Fatalf("deferred markers = %d, %v; want the cycle's seed marked", deferred, err)
	}
	// The budget returns and the marker falls due; the loop's sweep resolves it.
	r.noBudget.Store(false)
	if _, err := r.pool.Exec(t.Context(), `UPDATE control.blocked_record SET verify_after = now() - interval '1 second' WHERE verify_after IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	r.deliver(t)
	escalations := map[string]int{}
	for _, u := range updates(r) {
		if slices.Contains(u.AddLabels, providers.LabelNeedsHuman) && u.Comment != "" {
			if u.IdempotencyKey == "" || !strings.HasPrefix(u.IdempotencyKey, "cycle-") {
				t.Fatalf("an escalation comment without the cycle's key: %+v", u)
			}
			escalations[u.ID]++
		}
	}
	if escalations["42"] != 1 || escalations["43"] != 1 {
		t.Fatalf("escalations = %v; want each member escalated exactly once (updates %+v, reported %v)", escalations, updates(r), r.reported)
	}
	var wrote, summarized bool
	r.log.mu.Lock()
	for _, ev := range r.log.events {
		wrote = wrote || ev.Runner["kind"] == terminalwriter.InstanceWriteOperation
		summarized = summarized || ev.Runner["kind"] == "blocked_cycle_escalated"
	}
	r.log.mu.Unlock()
	if !wrote || !summarized {
		t.Fatalf("instance writes journaled %v, escalation summary journaled %v; want both", wrote, summarized)
	}
	before := len(updates(r))
	r.deliver(t) // nothing is owed any more
	if after := len(updates(r)); after != before {
		t.Fatalf("a settled cycle was written again: %d -> %d updates", before, after)
	}
	if st := r.comp.Delivery.Status(); st.DeferredCycles != 0 {
		t.Fatalf("status still shows %d deferred cycles", st.DeferredCycles)
	}
}

// Without the instance-level escalator there is no driver, so the composition must
// not defer: an unverifiable cycle is escalated at once, labelled, never parked with
// nobody to escalate it.
func TestWithoutAnEscalatorTheCompositionEscalatesAnUnverifiableCycleAtOnce(t *testing.T) {
	blockedOn := func(on string) engine.RunResult {
		return engine.RunResult{
			Status: engine.StatusEscalated, FinalState: "implement",
			Outputs: map[string]apiv1.ResultEnvelope{"implement": {Status: apiv1.ResultBlocked, Summary: "waits", Outputs: map[string]any{"blockedBy": on}}},
		}
	}
	tm := temporal{"run-a": {id: "run-a", result: blockedOn("43")}, "run-b": {id: "run-b", result: blockedOn("42")}}
	r := newRigFull(t, tm, map[string][]string{"run-a": {"42"}, "run-b": {"43"}},
		terminalcompose.Tuning{DeliveryEvery: time.Millisecond, HealEvery: time.Millisecond}, true)
	r.noBudget.Store(true)
	r.run(t)
	var deferred int
	if err := r.controller.QueryRow(t.Context(), `SELECT count(*) FROM control.blocked_record WHERE verify_after IS NOT NULL`).Scan(&deferred); err != nil || deferred != 0 {
		t.Fatalf("deferred markers = %d, %v; a composition without a driver must not defer", deferred, err)
	}
	escalated := map[string]bool{}
	for _, u := range updates(r) {
		if slices.Contains(u.AddLabels, providers.LabelNeedsHuman) {
			escalated[u.ID] = true
		}
	}
	if !escalated["42"] || !escalated["43"] {
		t.Fatalf("escalated = %v; want both members escalated at once (updates %+v, reported %v)", escalated, updates(r), r.reported)
	}
}
