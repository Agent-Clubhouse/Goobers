package localscheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type quotaReportingBacklogCounter struct {
	*fakeBacklogCounter
	quota     *ProviderQuotaState
	provider  apiv1.Provider
	remaining int
	resetAt   time.Time
}

type budgetExhaustedBacklogCounter struct {
	err error
}

func (c budgetExhaustedBacklogCounter) EligibleCount(context.Context) (int, error) {
	return 0, c.err
}

type cachedBacklogCounter struct {
	*fakeBacklogCounter
}

func (cachedBacklogCounter) ProviderQuotaGuarded() bool {
	return true
}

func (c *quotaReportingBacklogCounter) EligibleCount(ctx context.Context) (int, error) {
	count, err := c.fakeBacklogCounter.EligibleCount(ctx)
	c.quota.Record(c.provider, c.remaining, c.resetAt)
	return count, err
}

func TestTickShedsLowestPriorityProviderPolls(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(time.Hour)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 1, resetAt)

	high := &fakeBacklogCounter{}
	medium := &fakeBacklogCounter{}
	low := &fakeBacklogCounter{}
	ado := &fakeBacklogCounter{}
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "low", BacklogCounter: low, PollProvider: apiv1.ProviderGitHub, PollPriority: 1, Starter: starter},
		{Workflow: "high", BacklogCounter: high, PollProvider: apiv1.ProviderGitHub, PollPriority: 100, Starter: starter},
		{Workflow: "medium", BacklogCounter: medium, PollProvider: apiv1.ProviderGitHub, PollPriority: 10, Starter: starter},
		{Workflow: "ado", BacklogCounter: ado, PollProvider: apiv1.ProviderADO, PollPriority: 0, Starter: starter},
	}, WithProviderQuota(quota))

	sched.Tick(context.Background(), now)

	if high.polls() != 1 {
		t.Fatalf("high-priority polls = %d, want 1", high.polls())
	}
	if medium.polls() != 0 || low.polls() != 0 {
		t.Fatalf("lower-priority polls medium=%d low=%d, want both shed", medium.polls(), low.polls())
	}
	if ado.polls() != 1 {
		t.Fatalf("independent ADO polls = %d, want 1", ado.polls())
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	shed := map[string]bool{}
	for _, event := range events {
		if event.Type != journal.EventPollShed {
			continue
		}
		shed[event.Workflow] = true
		if !strings.Contains(event.Reason, ReasonProviderQuotaBudget) ||
			!strings.Contains(event.Reason, "provider=github") ||
			!strings.Contains(event.Reason, "reset="+resetAt.UTC().Format(time.RFC3339)) {
			t.Fatalf("poll shed reason is not inspectable: %q", event.Reason)
		}
	}
	if len(shed) != 2 || !shed["medium"] || !shed["low"] {
		t.Fatalf("shed workflows = %v, want medium and low", shed)
	}
}

func TestTickRechecksQuotaAfterPaginatedPoll(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(time.Hour)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 2, resetAt)

	high := &quotaReportingBacklogCounter{
		fakeBacklogCounter: &fakeBacklogCounter{},
		quota:              quota,
		provider:           apiv1.ProviderGitHub,
		remaining:          0,
		resetAt:            resetAt,
	}
	low := &fakeBacklogCounter{}
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "low", BacklogCounter: low, PollProvider: apiv1.ProviderGitHub, PollPriority: 1, Starter: starter},
		{Workflow: "high", BacklogCounter: high, PollProvider: apiv1.ProviderGitHub, PollPriority: 100, Starter: starter},
	}, WithProviderQuota(quota))

	sched.Tick(context.Background(), now)

	if high.polls() != 1 {
		t.Fatalf("high-priority polls = %d, want 1", high.polls())
	}
	if low.polls() != 0 {
		t.Fatalf("low-priority polls = %d, want shed after high-priority pagination exhausted quota", low.polls())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventPollShed && event.Workflow == "low" {
			if !strings.Contains(event.Reason, "remaining=0") {
				t.Fatalf("poll shed reason = %q, want exhausted quota cause", event.Reason)
			}
			return
		}
	}
	t.Fatal("low-priority pagination shed was not journaled")
}

func TestTickPacesPollingAfterQuotaReset(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(time.Minute)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)

	first := &fakeBacklogCounter{}
	second := &fakeBacklogCounter{}
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{
		{Workflow: "first", BacklogCounter: first, PollProvider: apiv1.ProviderGitHub, Starter: starter},
		{Workflow: "second", BacklogCounter: second, PollProvider: apiv1.ProviderGitHub, Starter: starter},
	}, WithProviderQuota(quota))

	sched.Tick(context.Background(), resetAt)

	if first.polls()+second.polls() != 1 {
		t.Fatalf("polls on reset tick first=%d second=%d, want one workflow admitted", first.polls(), second.polls())
	}
	sched.Tick(context.Background(), resetAt.Add(time.Second))
	if first.polls() != 1 || second.polls() != 1 {
		t.Fatalf("polls after paced tick first=%d second=%d, want both admitted across separate ticks", first.polls(), second.polls())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	resets := 0
	for _, event := range events {
		if event.Type == journal.EventProviderQuotaReset {
			resets++
			if !strings.Contains(event.Reason, "provider=github") ||
				!strings.Contains(event.Reason, "reset="+resetAt.UTC().Format(time.RFC3339)) {
				t.Fatalf("quota reset reason is not inspectable: %q", event.Reason)
			}
		}
	}
	if resets != 1 {
		t.Fatalf("provider quota reset events = %d, want 1", resets)
	}
}

func TestRunPacesOrdinaryScheduledWorkflowsAfterQuotaReset(t *testing.T) {
	resetAt := time.Date(2026, time.August, 7, 16, 5, 0, 0, time.UTC)
	clock := newFakeClock(resetAt)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)
	first := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	second := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:  "first",
			Schedules: []Schedule{fakeSchedule{d: time.Hour}},
			RepoRef:   apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
			Starter:   first,
		},
		{
			Workflow:  "second",
			Schedules: []Schedule{fakeSchedule{d: time.Hour}},
			RepoRef:   apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
			Starter:   second,
		},
	}, WithProviderQuota(quota), WithClock(clock.Now, clock.After))
	sched.mu.Lock()
	for identity, state := range sched.triggers {
		state.LastEval = resetAt.Add(-time.Hour)
		sched.triggers[identity] = state
	}
	sched.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context cancellation", err)
		}
		sched.Wait()
	})

	clock.awaitAfterCall(t)
	waitForCount(t, func() int { return first.count() + second.count() }, 1)
	if got := first.count() + second.count(); got != 1 {
		t.Fatalf("dispatches on reset tick = %d, want 1", got)
	}
	if waits := clock.durations(); len(waits) == 0 || waits[0] != minPoll {
		t.Fatalf("first paced wait = %v, want %s", waits, minPoll)
	}

	clock.advance(resetAt.Add(minPoll))
	waitForCount(t, func() int { return first.count() + second.count() }, 2)
	if first.count() != 1 || second.count() != 1 {
		t.Fatalf("paced dispatches first=%d second=%d, want one each", first.count(), second.count())
	}
}

func TestRunWakesForDeferredScheduleDemandPollWhenFirstPollIsEmpty(t *testing.T) {
	resetAt := time.Date(2026, time.August, 7, 16, 5, 0, 0, time.UTC)
	clock := newFakeClock(resetAt)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)
	firstCounter := &fakeBacklogCounter{}
	secondCounter := &fakeBacklogCounter{count: 1}
	first := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	second := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, _ := newTestScheduler(t, []WorkflowEntry{
		{
			Workflow:              "first",
			Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
			ScheduleDemandCounter: firstCounter,
			PollProvider:          apiv1.ProviderGitHub,
			RepoRef:               apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
			Starter:               first,
		},
		{
			Workflow:              "second",
			Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
			ScheduleDemandCounter: secondCounter,
			PollProvider:          apiv1.ProviderGitHub,
			RepoRef:               apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
			Starter:               second,
		},
	}, WithProviderQuota(quota), WithClock(clock.Now, clock.After))
	sched.mu.Lock()
	for identity, state := range sched.triggers {
		state.LastEval = resetAt.Add(-time.Hour)
		sched.triggers[identity] = state
	}
	sched.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run() error = %v, want context cancellation", err)
		}
		sched.Wait()
	})

	clock.awaitAfterCall(t)
	if got := firstCounter.polls() + secondCounter.polls(); got != 1 {
		t.Fatalf("demand polls on reset tick = %d, want 1", got)
	}
	if waits := clock.durations(); len(waits) == 0 || waits[0] != minPoll {
		t.Fatalf("wait after empty demand poll = %v, want %s", waits, minPoll)
	}
	clock.advance(resetAt.Add(minPoll))
	waitForCount(t, func() int { return firstCounter.polls() + secondCounter.polls() }, 2)
	waitForCount(t, second.count, 1)
	if first.count() != 0 || second.count() != 1 {
		t.Fatalf("paced dispatches first=%d second=%d, want only second dispatched", first.count(), second.count())
	}
}

func TestTickJournalsPaginationBudgetShed(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(time.Hour)
	counter := budgetExhaustedBacklogCounter{err: &ProviderPollBudgetError{
		Provider:  apiv1.ProviderGitHub,
		Remaining: 0,
		Requested: 1,
		ResetAt:   resetAt,
	}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "high",
		BacklogCounter: counter,
		PollProvider:   apiv1.ProviderGitHub,
		PollPriority:   100,
		Starter:        &fakeStarter{},
	}})

	sched.Tick(context.Background(), now)

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventPollShed && event.Workflow == "high" &&
			strings.Contains(event.Reason, "remaining=0") &&
			strings.Contains(event.Reason, "reset="+resetAt.UTC().Format(time.RFC3339)) {
			return
		}
	}
	t.Fatal("pagination budget shed was not journaled with its cause")
}

func TestManualDispatchJournalsQuotaReset(t *testing.T) {
	resetAt := time.Now().Add(time.Minute)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow: "manual",
		Starter:  starter,
		RepoRef:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub},
	}}, WithProviderQuota(quota))

	if _, err := sched.Trigger(context.Background(), "manual", resetAt); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, starter.count, 1)
	sched.Wait()

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	resets := 0
	for _, event := range events {
		if event.Type == journal.EventProviderQuotaReset {
			resets++
		}
	}
	if resets != 1 {
		t.Fatalf("provider quota reset events = %d, want 1 for admission-only reset", resets)
	}
}

func TestTickUsesGuardedSnapshotAtZeroQuota(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(time.Hour)
	quota := NewProviderQuotaState()
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)
	counter := cachedBacklogCounter{fakeBacklogCounter: &fakeBacklogCounter{}}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:       "cached",
		BacklogCounter: counter,
		PollProvider:   apiv1.ProviderGitHub,
		Starter:        &fakeStarter{},
	}}, WithProviderQuota(quota))

	sched.Tick(context.Background(), now)

	if counter.polls() != 1 {
		t.Fatalf("guarded cache probes = %d, want 1 at zero provider quota", counter.polls())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == journal.EventPollShed {
			t.Fatalf("guarded cache probe was shed before consulting its snapshot: %+v", event)
		}
	}
}
