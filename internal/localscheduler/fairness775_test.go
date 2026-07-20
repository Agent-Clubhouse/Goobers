package localscheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type releaseGate struct {
	ch   chan struct{}
	once sync.Once
}

func newReleaseGate(t *testing.T) *releaseGate {
	t.Helper()
	gate := &releaseGate{ch: make(chan struct{})}
	t.Cleanup(gate.release)
	return gate
}

func (g *releaseGate) release() {
	g.once.Do(func() { close(g.ch) })
}

func occupyWorkflow(t *testing.T, sched *Scheduler, identity WorkflowIdentity, readiness apiv1.ReadinessConditions, now time.Time) {
	t.Helper()
	if ok, reason := sched.conditions.AdmitWorkflow(identity, readiness, now); !ok {
		t.Fatalf("occupy %v: %s", identity, reason)
	}
	t.Cleanup(func() {
		sched.conditions.ReleaseWorkflow(identity)
	})
}

func TestTickBoundsFairDispatchAcrossGaggles(t *testing.T) {
	alphaGate := newReleaseGate(t)
	betaGate := newReleaseGate(t)
	gammaGate := newReleaseGate(t)
	alphaA := &fakeStarter{block: alphaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	alphaB := &fakeStarter{block: alphaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	alphaC := &fakeStarter{block: alphaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{block: betaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	gamma := &fakeStarter{block: gammaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}

	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "a-hot", BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: alphaA},
		{Gaggle: "alpha", Workflow: "b-hot", BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: alphaB},
		{Gaggle: "alpha", Workflow: "c-hot", BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: alphaC},
		{Gaggle: "beta", Workflow: "z-beta", BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: beta},
		{Gaggle: "gamma", Workflow: "zz-gamma", BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: gamma},
	})
	sched.conditions.SetInstanceLimits(1, nil, nil)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	want := []struct {
		gaggle string
		gate   *releaseGate
		count  func() int
	}{
		{gaggle: "alpha", gate: alphaGate, count: alphaA.count},
		{gaggle: "beta", gate: betaGate, count: beta.count},
		{gaggle: "gamma", gate: gammaGate, count: gamma.count},
	}
	for i, turn := range want {
		sched.Tick(context.Background(), now.Add(time.Duration(i)*(backlogPollInterval+time.Second)))
		waitForCount(t, turn.count, 1)

		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		var started []string
		for _, event := range events {
			if event.Type == journal.EventRunStarted {
				started = append(started, event.Gaggle)
			}
		}
		if len(started) != i+1 || started[i] != turn.gaggle {
			t.Fatalf("run.started gaggles after turn %d = %v, want prefix [alpha beta gamma]", i+1, started)
		}

		turn.gate.release()
		sched.Wait()
	}

	if got := alphaA.count() + alphaB.count() + alphaC.count(); got != 1 {
		t.Fatalf("hot gaggle starts = %d, want 1 before both peers receive their bounded turn", got)
	}
}

func TestTickPreservesTriggerEvaluationAndAfterTickAcrossGaggles(t *testing.T) {
	gate := newReleaseGate(t)
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	now := base.Add(time.Hour)
	alpha := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}

	var sched *Scheduler
	var dir string
	afterTickCalls := 0
	startedAtAfterTick := 0
	var afterTickErr error
	evaluated := make(map[WorkflowIdentity]time.Time)
	sched, dir = newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "deploy", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: alpha},
		{Gaggle: "beta", Workflow: "deploy", Schedules: []Schedule{fakeSchedule{d: time.Hour}}, Starter: beta},
	}, WithClock(func() time.Time { return base }, time.After), WithAfterTick(func(context.Context) {
		afterTickCalls++
		events, err := journal.ReadInstanceLog(dir)
		if err != nil {
			afterTickErr = err
			return
		}
		for _, event := range events {
			if event.Type == journal.EventRunStarted {
				startedAtAfterTick++
			}
		}
		sched.mu.Lock()
		defer sched.mu.Unlock()
		for identity, state := range sched.triggers {
			evaluated[identity] = state.LastEval
		}
	}))
	sched.conditions.SetInstanceLimits(1, nil, nil)

	sched.Tick(context.Background(), now)

	if afterTickCalls != 1 {
		t.Fatalf("after-tick calls = %d, want 1", afterTickCalls)
	}
	if afterTickErr != nil {
		t.Fatal(afterTickErr)
	}
	if startedAtAfterTick != 1 {
		t.Fatalf("run.started events observed after tick = %d, want 1", startedAtAfterTick)
	}
	for _, identity := range []WorkflowIdentity{
		{Gaggle: "alpha", Workflow: "deploy"},
		{Gaggle: "beta", Workflow: "deploy"},
	} {
		if got := evaluated[identity]; !got.Equal(now) {
			t.Fatalf("%v LastEval observed after tick = %v, want %v", identity, got, now)
		}
	}

	gate.release()
	sched.Wait()
}

func TestTickSkipsBlockedWorkflowsWithinGaggleTurn(t *testing.T) {
	gate := newReleaseGate(t)
	blockedReadiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 1}
	alphaReady := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "a-blocked", Readiness: blockedReadiness, BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: &fakeStarter{}},
		{Gaggle: "alpha", Workflow: "b-blocked", Readiness: blockedReadiness, BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: &fakeStarter{}},
		{Gaggle: "alpha", Workflow: "z-ready", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, BacklogCounter: &fakeBacklogCounter{count: 1}, Starter: alphaReady},
		{Gaggle: "beta", Workflow: "peer", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 2}, BacklogCounter: &fakeBacklogCounter{count: 2}, Starter: beta},
	})
	sched.conditions.SetInstanceLimits(4, nil, nil)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	occupyWorkflow(t, sched, WorkflowIdentity{Gaggle: "alpha", Workflow: "a-blocked"}, blockedReadiness, now.Add(-time.Minute))
	occupyWorkflow(t, sched, WorkflowIdentity{Gaggle: "alpha", Workflow: "b-blocked"}, blockedReadiness, now.Add(-time.Minute))

	sched.Tick(context.Background(), now)
	waitForCount(t, alphaReady.count, 1)
	waitForCount(t, beta.count, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			started = append(started, event.Gaggle)
		}
	}
	if len(started) != 2 || started[0] != "alpha" || started[1] != "beta" {
		t.Fatalf("run.started gaggles = %v, want [alpha beta]; blocked workflows must not spend alpha's turn", started)
	}

	gate.release()
	sched.Wait()
}

func TestTickInterleavesWorkflowFanoutByGaggle(t *testing.T) {
	gate := newReleaseGate(t)
	alphaA := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	alphaB := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Gaggle:         "alpha",
			Workflow:       "a-hot",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        alphaA,
		},
		{
			Gaggle:         "alpha",
			Workflow:       "b-hot",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        alphaB,
		},
		{
			Gaggle:         "beta",
			Workflow:       "z-peer",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        beta,
		},
	})
	sched.conditions.SetInstanceLimits(6, nil, nil)

	sched.Tick(context.Background(), time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	waitForCount(t, alphaA.count, 2)
	waitForCount(t, alphaB.count, 2)
	waitForCount(t, beta.count, 2)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			started = append(started, event.Workflow)
		}
	}
	want := []string{"a-hot", "z-peer", "b-hot", "z-peer", "a-hot", "b-hot"}
	if len(started) != len(want) {
		t.Fatalf("run.started workflow order = %v, want %v", started, want)
	}
	for i := range want {
		if started[i] != want[i] {
			t.Fatalf("run.started workflow order = %v, want %v", started, want)
		}
	}

	gate.release()
	sched.Wait()
}

func TestTickQuietGaggleDoesNotReserveCapacity(t *testing.T) {
	gate := newReleaseGate(t)
	hot := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	quiet := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{
			Gaggle:         "hot",
			Workflow:       "implement",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
			BacklogCounter: &fakeBacklogCounter{count: 3},
			Starter:        hot,
		},
		{
			Gaggle:         "quiet",
			Workflow:       "curate",
			BacklogCounter: &fakeBacklogCounter{},
			Starter:        quiet,
		},
	})
	sched.conditions.SetInstanceLimits(3, nil, nil)

	sched.Tick(context.Background(), time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	waitForCount(t, hot.count, 3)
	if got := quiet.count(); got != 0 {
		t.Fatalf("quiet gaggle starts = %d, want 0", got)
	}

	gate.release()
	sched.Wait()
}

func TestSignalUsesFairGaggleCursor(t *testing.T) {
	alphaAGate := newReleaseGate(t)
	alphaBGate := newReleaseGate(t)
	betaGate := newReleaseGate(t)
	alphaA := &fakeStarter{block: alphaAGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	alphaB := &fakeStarter{block: alphaBGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeStarter{block: betaGate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "a-hot", Signals: []string{"ready"}, Starter: alphaA},
		{Gaggle: "alpha", Workflow: "b-hot", Signals: []string{"ready"}, Starter: alphaB},
		{Gaggle: "beta", Workflow: "z-beta", Signals: []string{"ready"}, Starter: beta},
	})
	sched.conditions.SetInstanceLimits(1, nil, nil)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if runIDs := sched.Signal(context.Background(), "ready", now); len(runIDs) != 1 {
		t.Fatalf("first signal run IDs = %v, want one shared-capacity admission", runIDs)
	}
	waitForCount(t, alphaA.count, 1)
	alphaAGate.release()
	sched.Wait()

	if runIDs := sched.Signal(context.Background(), "ready", now.Add(time.Minute)); len(runIDs) != 1 {
		t.Fatalf("second signal run IDs = %v, want one shared-capacity admission", runIDs)
	}
	waitForCount(t, beta.count, 1)
	if got := alphaB.count(); got != 0 {
		t.Fatalf("second alpha workflow starts = %d, want beta to receive the next gaggle turn", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			started = append(started, event.Gaggle)
		}
	}
	if len(started) != 2 || started[0] != "alpha" || started[1] != "beta" {
		t.Fatalf("signal run.started gaggles = %v, want [alpha beta]", started)
	}

	betaGate.release()
	sched.Wait()
}

func TestSignalSkipsBlockedWorkflowsWithinGaggleTurn(t *testing.T) {
	gate := newReleaseGate(t)
	blockedReadiness := apiv1.ReadinessConditions{MaxConcurrentRuns: 1}
	alphaReady := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	betaA := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	betaB := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Gaggle: "alpha", Workflow: "a-blocked", Readiness: blockedReadiness, Signals: []string{"ready"}, Starter: &fakeStarter{}},
		{Gaggle: "alpha", Workflow: "b-blocked", Readiness: blockedReadiness, Signals: []string{"ready"}, Starter: &fakeStarter{}},
		{Gaggle: "alpha", Workflow: "z-ready", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"ready"}, Starter: alphaReady},
		{Gaggle: "beta", Workflow: "a-peer", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"ready"}, Starter: betaA},
		{Gaggle: "beta", Workflow: "b-peer", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}, Signals: []string{"ready"}, Starter: betaB},
	})
	sched.conditions.SetInstanceLimits(4, nil, nil)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	occupyWorkflow(t, sched, WorkflowIdentity{Gaggle: "alpha", Workflow: "a-blocked"}, blockedReadiness, now.Add(-time.Minute))
	occupyWorkflow(t, sched, WorkflowIdentity{Gaggle: "alpha", Workflow: "b-blocked"}, blockedReadiness, now.Add(-time.Minute))

	if runIDs := sched.Signal(context.Background(), "ready", now); len(runIDs) != 2 {
		t.Fatalf("signal run IDs = %v, want one admission per ready gaggle", runIDs)
	}
	waitForCount(t, alphaReady.count, 1)
	waitForCount(t, betaA.count, 1)
	if got := betaB.count(); got != 0 {
		t.Fatalf("second beta workflow starts = %d, want alpha to retain its bounded turn", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			started = append(started, event.Gaggle)
		}
	}
	if len(started) != 2 || started[0] != "alpha" || started[1] != "beta" {
		t.Fatalf("signal run.started gaggles = %v, want [alpha beta]", started)
	}

	gate.release()
	sched.Wait()
}

func TestTickPreservesSingleGaggleWorkflowOrder(t *testing.T) {
	gate := newReleaseGate(t)
	first := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	second := &fakeStarter{block: gate.ch, result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{
			Gaggle:         "only",
			Workflow:       "a-first",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 2},
			BacklogCounter: &fakeBacklogCounter{count: 2},
			Starter:        first,
		},
		{
			Gaggle:         "only",
			Workflow:       "b-second",
			Readiness:      apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			BacklogCounter: &fakeBacklogCounter{count: 1},
			Starter:        second,
		},
	})
	sched.conditions.SetInstanceLimits(3, nil, nil)

	sched.Tick(context.Background(), time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))
	waitForCount(t, first.count, 2)
	waitForCount(t, second.count, 1)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventRunStarted {
			started = append(started, event.Workflow)
		}
	}
	want := []string{"a-first", "b-second", "a-first"}
	if len(started) != len(want) {
		t.Fatalf("single-gaggle run.started order = %v, want %v", started, want)
	}
	for i := range want {
		if started[i] != want[i] {
			t.Fatalf("single-gaggle run.started order = %v, want %v", started, want)
		}
	}

	gate.release()
	sched.Wait()
}
