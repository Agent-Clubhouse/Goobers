package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// recordingEngineStarter is an engine.Starter that records what it was asked
// to start and, optionally, blocks so the caller can observe the state of the
// world in the window between Start and the first emit.
type recordingEngineStarter struct {
	mu      sync.Mutex
	started []engine.RunInput
	err     error
	// gate, when non-nil, is closed by the test to release Start.
	gate chan struct{}
	// entered is closed the first time Start is called.
	entered chan struct{}
	once    sync.Once
}

func (s *recordingEngineStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	s.mu.Lock()
	s.started = append(s.started, in)
	s.mu.Unlock()
	s.once.Do(func() {
		if s.entered != nil {
			close(s.entered)
		}
	})
	if s.gate != nil {
		<-s.gate
	}
	return engine.StartResult{RunID: in.RunID}, s.err
}

func (s *recordingEngineStarter) inputs() []engine.RunInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.RunInput(nil), s.started...)
}

// engineStarterFixture builds one engineStarter over a real live-journal
// writer and instance log rooted in a temp layout, plus the fakes it
// dispatches through.
type engineStarterFixture struct {
	starter  *engineStarter
	engine   *recordingEngineStarter
	temporal *fakeEngineWorkflows
	live     *livejournal.Writer
	layout   instance.Layout
	logDir   string
	hooks    *hookRecorder
}

func newEngineStarterFixture(t *testing.T, temporal *fakeEngineWorkflows, engineStart *recordingEngineStarter) *engineStarterFixture {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatalf("open instance log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	runsDir := layout.RunsDir()
	live, err := livejournal.NewWriter(func(string) (string, bool) { return runsDir, true })
	if err != nil {
		t.Fatalf("new live journal writer: %v", err)
	}
	t.Cleanup(live.Close)

	rec := &hookRecorder{}
	runtime := &engineRuntime{}
	runtime.Attach(engineStart, &engineRunGuards{client: temporal}, live, func() time.Time {
		return time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	})

	cfg, set, project := runControlsFixture()
	spec := engineRunRequestFor(t, cfg, set, "web", "implementation")
	spec.project = project
	def := spec.def
	return &engineStarterFixture{
		engine:   engineStart,
		temporal: temporal,
		live:     live,
		layout:   layout,
		logDir:   layout.SchedulerDir(),
		hooks:    rec,
		starter: &engineStarter{
			runtime:     runtime,
			hooks:       rec.hooks(log),
			gaggle:      "web",
			def:         def,
			spec:        spec,
			layout:      layout,
			log:         log,
			liveJournal: true,
		},
	}
}

func engineStartRequest(runID string) localscheduler.StartRequest {
	return localscheduler.StartRequest{
		RunID:        runID,
		Gaggle:       "web",
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:         &apiv1.BacklogItem{ID: "42", Title: "an item"},
		GooberDigest: "sha256:deadbeef",
	}
}

// TestEngineStarterRefusesWhenTheRuntimeIsNotAttached is the fail-closed
// property, and the reason engineRuntime is a late-bound holder rather than a
// nil-tolerant field.
//
// A lane the selection predicate placed on the engine has stages pinned to
// REMOTE runners. Falling back to the local runner because the Temporal client
// is missing would execute those stages on the daemon's host — the exact
// placement violation the pins exist to prevent, performed silently. Failing
// the dispatch is visible in the scheduler's terminal echo; a silent fallback
// is not.
func TestEngineStarterRefusesWhenTheRuntimeIsNotAttached(t *testing.T) {
	fixture := newEngineStarterFixture(t, &fakeEngineWorkflows{}, &recordingEngineStarter{})
	fixture.starter.runtime = &engineRuntime{} // never attached

	res, err := fixture.starter.Start(context.Background(), engineStartRequest("run-unattached"))
	if err == nil {
		t.Fatal("an unattached engine runtime dispatched a run; remotely-pinned stages would have executed on this host")
	}
	if !errors.Is(err, errEngineRuntimeUnattached) {
		t.Errorf("error = %v, want it to name the unattached runtime", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Errorf("phase = %q, want %q so the refusal is visible in the scheduler's terminal echo", res.Phase, journal.PhaseFailed)
	}
	if len(fixture.engine.inputs()) != 0 {
		t.Error("a workflow was started despite the unattached runtime")
	}
}

// TestEngineStarterReservesTheRunBeforeStartingTheWorkflow is the
// start-to-first-emit window closure (piece 6), asserted at the only instant
// it can be: INSIDE the engine Start call, before any emit could have
// happened.
//
// A daemon that crashed here without the reservation would leave the run in
// Temporal and nowhere else — no runs/<id>/ for the boot scan to find — and
// the next daemon would re-admit the same workflow under a fresh run id while
// the first kept executing, with the first run's terminal hooks never firing.
func TestEngineStarterReservesTheRunBeforeStartingTheWorkflow(t *testing.T) {
	engineStart := &recordingEngineStarter{
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	fixture := newEngineStarterFixture(t, &fakeEngineWorkflows{
		result: engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement"},
	}, engineStart)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = fixture.starter.Start(context.Background(), engineStartRequest("run-reserve"))
	}()

	select {
	case <-engineStart.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Start was never reached")
	}

	// The window. The workflow is being started RIGHT NOW and has emitted
	// nothing; runs/<id>/ must already exist and must already say the run is
	// the engine's.
	rd, err := journal.OpenRead(filepath.Join(fixture.layout.RunsDir(), "run-reserve"))
	if err != nil {
		t.Fatalf("no run directory inside the start-to-first-emit window: %v; a restart here re-admits the workflow under a fresh id while the first keeps executing", err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("reserved run has no readable identity: %v", err)
	}
	if !id.EngineDriven() {
		t.Errorf("reserved run.yaml driver = %q, want the engine driver so the boot scan reattaches instead of resuming in-process", id.Driver)
	}
	if id.RunID != "run-reserve" {
		t.Errorf("reserved run id = %q, want run-reserve", id.RunID)
	}
	if id.GooberDigest != "sha256:deadbeef" {
		t.Errorf("reserved run identity GooberDigest = %q, want the scheduler's pinned digest", id.GooberDigest)
	}

	close(engineStart.gate)
	<-done
}

// TestEngineStarterHoldsTheSchedulerSlotUntilTheRunIsTerminal is the slot
// retention hazard.
//
// localscheduler.Starter's contract is that the workflow's concurrency slot is
// held for exactly as long as Start runs. An engine dispatch is asynchronous —
// Temporal accepts the workflow and returns immediately — so a starter that
// returned after the dispatch would release the slot under a live run and
// journal a fabricated terminal, letting the scheduler admit a SECOND run of
// the same workflow.
func TestEngineStarterHoldsTheSchedulerSlotUntilTheRunIsTerminal(t *testing.T) {
	temporal := &fakeEngineWorkflows{
		gate:   make(chan struct{}),
		result: engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement"},
	}
	fixture := newEngineStarterFixture(t, temporal, &recordingEngineStarter{})

	returned := make(chan localscheduler.StartResult, 1)
	go func() {
		res, _ := fixture.starter.Start(context.Background(), engineStartRequest("run-slot"))
		returned <- res
	}()

	select {
	case res := <-returned:
		t.Fatalf("Start returned %+v while the workflow was still executing; the scheduler would admit a second run of the same workflow", res)
	case <-time.After(300 * time.Millisecond):
	}

	close(temporal.gate)
	select {
	case res := <-returned:
		if res.Phase != journal.PhaseCompleted {
			t.Errorf("phase = %q, want %q once the workflow closed", res.Phase, journal.PhaseCompleted)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start never returned after the workflow closed")
	}
}

// TestEngineStarterSurvivesRequestContextCancellation is the HTTP
// request-context hazard, reproduced end to end at the starter.
//
// A trigger-plane POST returns 200 as soon as the run is admitted, and Go's
// http.Server cancels the request context the instant the handler returns. If
// that context reached the engine dispatch, every webhook- and trigger-plane-
// started engine run would have its await cancelled while the workflow kept
// executing on the far side: the daemon would report a terminal the run never
// reached. The dispatch must run on a context whose cancellation means
// something about the RUN.
//
// Here the cancellation is delivered while the workflow is still open. The
// starter must NOT report a terminal phase, must NOT fire the terminal hooks
// (which release the run's claims), and must say the outcome is unknown.
func TestEngineStarterSurvivesRequestContextCancellation(t *testing.T) {
	temporal := &fakeEngineWorkflows{gate: make(chan struct{})}
	fixture := newEngineStarterFixture(t, temporal, &recordingEngineStarter{})
	t.Cleanup(func() { close(temporal.gate) })

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res localscheduler.StartResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := fixture.starter.Start(ctx, engineStartRequest("run-cancelled"))
		done <- outcome{res, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a cancelled dispatch context reported success; the workflow is still executing on the far side")
		}
		if got.res.Phase == journal.PhaseCompleted {
			t.Fatalf("phase = %q for a run whose outcome was never established", got.res.Phase)
		}
		if got.res.Phase != journal.PhaseRunning {
			t.Errorf("phase = %q, want %q: the run is presumed still executing and the boot scan reattaches to it", got.res.Phase, journal.PhaseRunning)
		}
		if len(fixture.hooks.order) != 0 {
			t.Errorf("terminal hooks %v fired for a run whose outcome is unknown; its claims would be released under a live workflow", fixture.hooks.order)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start never returned after the dispatch context was cancelled")
	}
}

// TestEngineStarterFiresTerminalHooksOnceTheWorkflowCloses is the positive
// arm: the whole point of the starter is that an engine run's instance-level
// side effects actually happen.
func TestEngineStarterFiresTerminalHooksOnceTheWorkflowCloses(t *testing.T) {
	temporal := &fakeEngineWorkflows{
		status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		result: engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement", NoWork: true},
	}
	fixture := newEngineStarterFixture(t, temporal, &recordingEngineStarter{})

	res, err := fixture.starter.Start(context.Background(), engineStartRequest("run-terminal"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Errorf("phase = %q, want %q", res.Phase, journal.PhaseCompleted)
	}
	if !res.NoWork {
		t.Error("NoWork was dropped on the way to the scheduler; an empty backlog would be re-ticked at full schedule rate")
	}
	want := []string{"prepare", "notify", "finalize"}
	if len(fixture.hooks.order) != len(want) {
		t.Fatalf("hook order = %v, want %v; a run that ends without the frame leaks its claims until the lease expires", fixture.hooks.order, want)
	}
	for i := range want {
		if fixture.hooks.order[i] != want[i] {
			t.Fatalf("hook order = %v, want %v", fixture.hooks.order, want)
		}
	}
}

// TestEngineStarterPinsTheSchedulersRunFacts proves the per-tick facts the
// scheduler supplies — run id, repo, item, trigger and goober digest — all
// reach the RunInput. A dispatch that dropped the item would run a
// backlog-item workflow with nothing to work on; one that dropped the digest
// would produce a run whose identity cannot name the goober it walked.
func TestEngineStarterPinsTheSchedulersRunFacts(t *testing.T) {
	temporal := &fakeEngineWorkflows{
		status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		result: engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement"},
	}
	fixture := newEngineStarterFixture(t, temporal, &recordingEngineStarter{})
	req := engineStartRequest("run-facts")
	req.Trigger = journal.Trigger{Kind: "item", Ref: "issue:42"}

	if _, err := fixture.starter.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	inputs := fixture.engine.inputs()
	if len(inputs) != 1 {
		t.Fatalf("engine started %d runs, want 1", len(inputs))
	}
	in := inputs[0]
	if in.RunID != "run-facts" || in.Gaggle != "web" {
		t.Errorf("RunInput identity = %s/%s, want web/run-facts", in.Gaggle, in.RunID)
	}
	if in.Item == nil || in.Item.ID != "42" {
		t.Errorf("RunInput.Item = %+v, want the scheduler's driving item", in.Item)
	}
	if in.GooberDigest != "sha256:deadbeef" {
		t.Errorf("RunInput.GooberDigest = %q, want the scheduler's pinned digest", in.GooberDigest)
	}
	if in.TriggerKind != "item" || in.TriggerRef != "issue:42" {
		t.Errorf("RunInput trigger = %s/%s, want item/issue:42", in.TriggerKind, in.TriggerRef)
	}
	if in.RepoRef.Owner != "acme" || in.RepoRef.Name != "web" {
		t.Errorf("RunInput.RepoRef = %+v, want the scheduler's repo", in.RepoRef)
	}
}

// TestEngineStarterClosesTheReservationWhenTheStartFails is the compensating
// half of the reservation, and it exists because an OPEN reservation is worse
// than no reservation at all.
//
// ReserveRun writes runs/<id>/ before Temporal is called, so a crash in the
// window leaves a record. A start that FAILS outright — an unreachable
// frontend, a namespace error, a deadline — leaves that same record with no
// workflow that will ever finish it, and nothing reclaims it: the orphan
// pruner skips any directory holding a run.yaml, and the stalled-run sweep
// re-tries cancelling a workflow that does not exist on every tick, forever.
// During a Temporal outage that is one immortal `running` run per scheduler
// tick per engine lane.
func TestEngineStarterClosesTheReservationWhenTheStartFails(t *testing.T) {
	startErr := errors.New("temporal frontend unavailable")
	fixture := newEngineStarterFixture(t, &fakeEngineWorkflows{}, &recordingEngineStarter{err: startErr})

	res, err := fixture.starter.Start(context.Background(), engineStartRequest("run-startfail"))
	if err == nil {
		t.Fatal("a failed engine start reported success")
	}
	if res.Phase != journal.PhaseFailed {
		t.Errorf("phase = %q, want %q", res.Phase, journal.PhaseFailed)
	}

	dir := filepath.Join(fixture.layout.RunsDir(), "run-startfail")
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open reserved run: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("read reserved run events: %v", err)
	}
	var finished *journal.Event
	var cause string
	for i := range events {
		switch events[i].Type {
		case journal.EventRunFinished:
			finished = &events[i]
		case journal.EventError:
			if events[i].Error != nil && events[i].Error.Code == "run_failed" {
				cause = events[i].Error.Message
			}
		}
	}
	if finished == nil {
		t.Fatal("the reservation was left in phase running with no workflow that could ever finish it; the stalled-run sweep would retry cancelling a nonexistent workflow forever")
	}
	if finished.Status != string(journal.PhaseFailed) {
		t.Errorf("run.finished status = %q, want %q", finished.Status, journal.PhaseFailed)
	}
	if cause == "" || !strings.Contains(cause, startErr.Error()) {
		t.Errorf("run_failed cause = %q, want it to carry the start failure %q", cause, startErr)
	}
}

// TestEngineStarterDoesNotCloseARunItNeverReserved: with the live journal off
// there is no reservation, so the failure path must not conjure a run
// directory for a workflow that was never started.
func TestEngineStarterDoesNotCloseARunItNeverReserved(t *testing.T) {
	fixture := newEngineStarterFixture(t, &fakeEngineWorkflows{}, &recordingEngineStarter{err: errors.New("nope")})
	fixture.starter.liveJournal = false

	if _, err := fixture.starter.Start(context.Background(), engineStartRequest("run-noreserve")); err == nil {
		t.Fatal("a failed engine start reported success")
	}
	if _, err := journal.OpenRead(filepath.Join(fixture.layout.RunsDir(), "run-noreserve")); err == nil {
		t.Error("a run directory was created for a dispatch that never reserved one")
	}
}

// TestEngineRuntimeAdoptFromCarriesTheBootAttachment is the config-reload
// hazard. buildSchedulerDefinitions mints a FRESH holder on every call and a
// config reload calls it again, so without adoption every reloaded engine
// lane's Starter would point at a never-attached runtime and fail closed on
// every dispatch until the daemon restarted — silently, because failing
// closed is the right answer to a genuinely unattached runtime. That would
// make a reload, the very operation the per-lane rollback story depends on,
// the one thing that breaks the engine path.
func TestEngineRuntimeAdoptFromCarriesTheBootAttachment(t *testing.T) {
	boot := &engineRuntime{}
	starter := &recordingEngineStarter{}
	guards := &engineRunGuards{client: &fakeEngineWorkflows{}}
	clock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	boot.Attach(starter, guards, nil, clock)

	reloaded := &engineRuntime{}
	if _, _, _, _, err := reloaded.resolve(); err == nil {
		t.Fatal("a fresh holder resolved; the fixture is not exercising the hazard")
	}
	reloaded.adoptFrom(boot)

	gotStarter, gotGuards, _, gotNow, err := reloaded.resolve()
	if err != nil {
		t.Fatalf("the reloaded runtime is unattached: %v; every engine lane would fail closed from this reload on", err)
	}
	if gotStarter != engine.Starter(starter) || gotGuards != guards {
		t.Error("the reloaded runtime adopted a different attachment than the boot one")
	}
	if !gotNow().Equal(clock()) {
		t.Error("the reloaded runtime did not adopt the boot clock")
	}

	// Adopting from an unattached holder must not clobber a good attachment,
	// and self-adoption must not deadlock on the RWMutex.
	reloaded.adoptFrom(&engineRuntime{})
	reloaded.adoptFrom(reloaded)
	reloaded.adoptFrom(nil)
	if _, _, _, _, err := reloaded.resolve(); err != nil {
		t.Fatalf("a good attachment was lost: %v", err)
	}
}
