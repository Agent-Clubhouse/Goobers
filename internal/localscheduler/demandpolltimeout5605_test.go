package localscheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// deadlineCounter fails every poll the way #5605's slow pr-remediation
// counter did: the scan outlives demandPollTimeout and returns the poll
// context's deadline error.
type deadlineCounter struct {
	fakeBacklogCounter
}

func (c *deadlineCounter) EligibleCount(ctx context.Context) (int, error) {
	c.mu.Lock()
	c.polled++
	c.mu.Unlock()
	<-ctx.Done()
	return 0, fmt.Errorf("check blocked-on-sibling state for PR #61: %w", ctx.Err())
}

func tickDueSchedule(t *testing.T, sched *Scheduler, workflow string, after time.Duration) {
	t.Helper()
	sched.mu.Lock()
	lastEval := sched.triggers[WorkflowIdentity{Workflow: workflow}].LastEval
	sched.mu.Unlock()
	sched.Tick(context.Background(), lastEval.Add(after))
}

func countErrorCode(t *testing.T, dir, code string) int {
	t.Helper()
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == code {
			n++
		}
	}
	return n
}

func starvedReasons(t *testing.T, dir string) []string {
	t.Helper()
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, event := range events {
		if event.Type == journal.EventWorkflowStarved {
			reasons = append(reasons, event.Reason)
		}
	}
	return reasons
}

// A schedule demand poll that times out fires exactly one run instead of
// consuming the cron slot as zero demand (#5605), even with capacity for more.
func TestScheduleDemandPollTimeoutFiresOneRun(t *testing.T) {
	starter := &fakeStarter{result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &deadlineCounter{}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 3},
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}})
	sched.demandPollTimeout = 20 * time.Millisecond

	tickDueSchedule(t, sched, "pr-remediation", time.Hour)
	sched.Wait()

	if counter.polls() != 1 {
		t.Fatalf("demand polls = %d, want 1", counter.polls())
	}
	if starter.count() != 1 {
		t.Fatalf("scheduled runs after timed-out demand poll = %d, want exactly 1", starter.count())
	}
	if got := countErrorCode(t, dir, "schedule_demand_count_failed"); got != 1 {
		t.Fatalf("schedule_demand_count_failed events = %d, want 1", got)
	}
}

// The fallback run is still subject to readiness admission: with one run in
// flight and maxConcurrentRuns 1, a second timed-out poll starts nothing.
func TestScheduleDemandPollTimeoutRespectsAdmission(t *testing.T) {
	block := make(chan struct{})
	starter := &fakeStarter{block: block, result: StartResult{Phase: journal.PhaseCompleted}}
	counter := &deadlineCounter{}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow:              "pr-remediation",
		Readiness:             apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Schedules:             []Schedule{fakeSchedule{d: time.Hour}},
		ScheduleDemandCounter: counter,
		Starter:               starter,
	}})
	sched.demandPollTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		close(block)
		sched.Wait()
	})

	tickDueSchedule(t, sched, "pr-remediation", time.Hour)
	waitForCount(t, starter.count, 1)
	tickDueSchedule(t, sched, "pr-remediation", time.Hour)

	if counter.polls() != 2 {
		t.Fatalf("demand polls = %d, want 2", counter.polls())
	}
	if starter.count() != 1 {
		t.Fatalf("runs = %d, want the fallback held to maxConcurrentRuns 1", starter.count())
	}
	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var skipped bool
	for _, event := range events {
		if event.Type == journal.EventTickSkipped && strings.Contains(event.Reason, "max-parallel") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("second fallback fire was not refused by readiness: %+v", events)
	}
}

// failedPollDemand fires only for a schedule poll whose count is unknown for
// a transient reason. Budget and auth errors, rate-limit give-ups, persistent
// failures, a cancelled parent, and every backlog/refill poll stay at 0.
func TestPollDemandFailureClassification(t *testing.T) {
	deadline := fmt.Errorf("check blocked-on-sibling state for PR #61: %w", context.DeadlineExceeded)
	cases := []struct {
		name string
		poll demandPoll
		err  error
		want int
	}{
		{"schedule timeout", demandPoll{schedule: true}, deadline, 1},
		{"schedule 5xx", demandPoll{schedule: true}, errors.New("GET /pulls failed: status 502"), 1},
		{"schedule budget", demandPoll{schedule: true}, &ProviderPollBudgetError{Provider: apiv1.ProviderGitHub}, 0},
		{"schedule auth", demandPoll{schedule: true}, errors.New("GET /pulls failed: status 401"), 0},
		{"schedule rate limit", demandPoll{schedule: true}, fmt.Errorf("list: %w", &providers.RateLimitError{Status: 429}), 0},
		{"schedule persistent", demandPoll{schedule: true}, errors.New("GET /pulls failed: status 404"), 0},
		{"backlog timeout", demandPoll{}, deadline, 0},
		{"refill timeout", demandPoll{refill: true}, deadline, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched, _ := newTestScheduler(t, nil)
			tc.poll.counter = &fakeBacklogCounter{err: tc.err}
			entry := WorkflowEntry{Workflow: "pr-remediation"}
			if got := sched.pollDemand(context.Background(), entry, tc.poll); got != tc.want {
				t.Fatalf("pollDemand() = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("cancelled parent", func(t *testing.T) {
		sched, _ := newTestScheduler(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		poll := demandPoll{schedule: true, counter: &fakeBacklogCounter{err: deadline}}
		if got := sched.pollDemand(ctx, WorkflowEntry{Workflow: "pr-remediation"}, poll); got != 0 {
			t.Fatalf("pollDemand() under shutdown = %d, want 0", got)
		}
	})
}

// demandPollFailureThreshold consecutive failures journal one
// workflow.starved event; further failures stay quiet, a successful poll
// re-arms the alarm, and each poll kind keeps its own streak.
func TestConsecutiveDemandPollFailuresJournalOneStarvedEvent(t *testing.T) {
	sched, dir := newTestScheduler(t, nil)
	entry := WorkflowEntry{Workflow: "pr-remediation", Gaggle: "goobers"}
	failing := demandPoll{schedule: true, counter: &fakeBacklogCounter{err: context.DeadlineExceeded}}
	healthy := demandPoll{schedule: true, counter: &fakeBacklogCounter{count: 2}}
	healthyBacklog := demandPoll{counter: &fakeBacklogCounter{count: 2}}
	ctx := context.Background()

	for i := 1; i < demandPollFailureThreshold; i++ {
		sched.pollDemand(ctx, entry, failing)
		// A healthy poll of another kind must not reset the schedule streak.
		sched.pollDemand(ctx, entry, healthyBacklog)
	}
	if got := starvedReasons(t, dir); len(got) != 0 {
		t.Fatalf("starved events below threshold = %v, want none", got)
	}
	sched.pollDemand(ctx, entry, failing)
	sched.pollDemand(ctx, entry, failing)
	reasons := starvedReasons(t, dir)
	if len(reasons) != 1 {
		t.Fatalf("starved events = %v, want exactly 1 per streak", reasons)
	}
	if !strings.Contains(reasons[0], "schedule_demand_count_failed") {
		t.Fatalf("starved reason = %q, want the failing poll kind named", reasons[0])
	}

	if got := sched.pollDemand(ctx, entry, healthy); got != 2 {
		t.Fatalf("healthy poll = %d, want 2", got)
	}
	for i := 0; i < demandPollFailureThreshold; i++ {
		sched.pollDemand(ctx, entry, failing)
	}
	if got := starvedReasons(t, dir); len(got) != 2 {
		t.Fatalf("starved events after reset and a new streak = %v, want 2", got)
	}
}
