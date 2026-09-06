package localscheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// sequencedStarter returns NoWork per call according to noWork, which the
// test can flip between deliveries to simulate a webhook burst that finds no
// eligible work followed by one that does.
type sequencedStarter struct {
	mu     sync.Mutex
	starts int
	noWork bool
}

func (s *sequencedStarter) Start(context.Context, StartRequest) (StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	return StartResult{Phase: journal.PhaseCompleted, NoWork: s.noWork}, nil
}

func (s *sequencedStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func (s *sequencedStarter) setNoWork(noWork bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noWork = noWork
}

// TestWebhookIdleBackoffBoundsBurstDispatchVolume is the #4262 regression: a
// burst of webhook deliveries that each find no eligible work (merge-review's
// pr-select finding no PR, repeatedly) must not dispatch one run per
// delivery — that is exactly the fleet-wide provider-quota exhaustion the
// issue reports. Idle backoff must bound the run count the way it already
// does for schedule triggers.
func TestWebhookIdleBackoffBoundsBurstDispatchVolume(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &sequencedStarter{noWork: true}
	sched, dir := newTestScheduler(t, []WorkflowEntry{{
		Workflow: "pr-select", Gaggle: "goobers",
		Signals: []string{webhookhttp.SignalName("pull_request")},
		WebhookBackoff: IdleBackoffConfig{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	// A burst of 20 pull_request deliveries within the same second — the
	// shape of a hot PR generating many webhook events — must not each
	// dispatch a run once the first no-work outcome has recorded.
	for i := 0; i < 20; i++ {
		delivery := webhookhttp.Delivery{Event: "pull_request", ID: strings.Repeat("d", i+1)}
		sched.SignalWebhook(context.Background(), delivery, now)
		sched.Wait()
	}

	if got := starter.count(); got != 1 {
		t.Fatalf("starts after 20-delivery burst = %d, want 1 (idle backoff must suppress the remaining 19)", got)
	}

	// Advance past the floor interval and re-deliver: still no work, so the
	// backoff engages again after this one dispatch — bounded growth, not a
	// fixed suppression that would delay reaction to real activity forever.
	now = base.Add(90 * time.Second)
	sched.SignalWebhook(context.Background(), webhookhttp.Delivery{Event: "pull_request", ID: "after-floor"}, now)
	sched.Wait()
	if got := starter.count(); got != 2 {
		t.Fatalf("starts after floor elapsed = %d, want 2", got)
	}

	// A second immediate burst right after must again be fully suppressed.
	for i := 0; i < 10; i++ {
		delivery := webhookhttp.Delivery{Event: "pull_request", ID: "second-burst-" + strings.Repeat("d", i+1)}
		sched.SignalWebhook(context.Background(), delivery, now)
		sched.Wait()
	}
	if got := starter.count(); got != 2 {
		t.Fatalf("starts after second burst = %d, want still 2", got)
	}

	events, err := journal.ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == journal.EventTickSkipped && strings.HasPrefix(event.Reason, "idle backoff:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("webhook idle backoff suppression was not journaled")
	}
}

// TestWebhookIdleBackoffResetsOnFirstRealWork mirrors resetIdleBackoff's
// existing schedule-trigger contract (#4262's acceptance criteria require
// the same reset-on-real-work behavior): once a webhook-triggered run
// reports real work, backoff must not carry over and delay reacting to the
// very next delivery.
func TestWebhookIdleBackoffResetsOnFirstRealWork(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &sequencedStarter{noWork: true}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow: "pr-select", Gaggle: "goobers",
		Signals: []string{webhookhttp.SignalName("pull_request")},
		WebhookBackoff: IdleBackoffConfig{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	sched.SignalWebhook(context.Background(), webhookhttp.Delivery{Event: "pull_request", ID: "d1"}, now)
	sched.Wait()
	if got := starter.count(); got != 1 {
		t.Fatalf("starts after first delivery = %d, want 1", got)
	}

	// A delivery arriving inside the backoff window is correctly suppressed
	// pre-dispatch — its outcome isn't known yet, so it can't be the "event
	// that finds real work" the reset contract talks about.
	starter.setNoWork(false)
	sched.SignalWebhook(context.Background(), webhookhttp.Delivery{Event: "pull_request", ID: "d2-suppressed"}, now)
	sched.Wait()
	if got := starter.count(); got != 1 {
		t.Fatalf("starts on delivery still inside the backoff window = %d, want 1 (unknown outcome must not bypass backoff)", got)
	}

	// Once backoff naturally releases (nextPoll reached) and that dispatch
	// finds real work, the reset must be immediate: the very next delivery,
	// seconds later — well inside what would otherwise be the next backoff
	// interval — must not be suppressed.
	now = base.Add(70 * time.Second)
	sched.SignalWebhook(context.Background(), webhookhttp.Delivery{Event: "pull_request", ID: "d3-real-work"}, now)
	sched.Wait()
	if got := starter.count(); got != 2 {
		t.Fatalf("starts once backoff released and real work found = %d, want 2", got)
	}

	starter.setNoWork(true)
	now = base.Add(75 * time.Second)
	sched.SignalWebhook(context.Background(), webhookhttp.Delivery{Event: "pull_request", ID: "d4-immediately-after"}, now)
	sched.Wait()
	if got := starter.count(); got != 3 {
		t.Fatalf("starts immediately after real work reset backoff = %d, want 3 (reset must be immediate, not gradual)", got)
	}
}

// TestSignalDoesNotApplyWebhookIdleBackoff scopes the fix to webhook
// deliveries specifically: an internal type=signal trigger (goobers signal
// CLI / another workflow's output) must keep its existing unthrottled
// behavior, since #4262 is about webhook-originated GitHub API load, not
// internal signal fan-out.
func TestSignalDoesNotApplyWebhookIdleBackoff(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	now := base
	starter := &sequencedStarter{noWork: true}
	sched, _ := newTestScheduler(t, []WorkflowEntry{{
		Workflow: "poll", Gaggle: "goobers",
		Signals: []string{"work-ready"},
		WebhookBackoff: IdleBackoffConfig{
			Enabled: true,
			Floor:   time.Minute,
			Ceiling: 4 * time.Minute,
		},
		Starter: starter,
	}}, WithClock(func() time.Time { return now }, time.After))

	for i := 0; i < 5; i++ {
		sched.Signal(context.Background(), "work-ready", now)
		sched.Wait()
	}

	if got := starter.count(); got != 5 {
		t.Fatalf("starts via internal Signal = %d, want 5 (webhook idle backoff must not apply)", got)
	}
}
