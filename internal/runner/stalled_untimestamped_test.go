package runner

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// An event written WITHOUT a timestamp says nothing about when activity
// happened. Reading its zero Time as "last active at year 1" makes the
// staleness comparison trivially true, so the run is escalated regardless of
// how large the timeout is — no configuration can prevent it.
//
// MEASURED (#3774): run 8238995d was escalated 11 minutes into a 60-minute
// agentic stage. Its newest event was agent.lifecycle, written with
// time 0001-01-01T00:00:00Z, while four correctly stamped events sat directly
// above it. Because an agentic stage emits agent.lifecycle and then works
// silently, ANY run whose newest event was that one died on the next sweep —
// long agentic work could not complete in mode 3 at all.
func TestNewestTimestampedSkipsUnstampedEvents(t *testing.T) {
	stamped := time.Date(2026, 8, 28, 2, 54, 11, 0, time.UTC)
	events := []journal.Event{
		{Type: journal.EventRunStarted, Time: stamped.Add(-time.Minute)},
		{Type: journal.EventStageStarted, Time: stamped},
		// The shape that broke it: newest event, no timestamp.
		{Type: journal.EventAgentLifecycle},
	}
	if got := newestTimestamped(events); !got.Equal(stamped) {
		t.Fatalf("newestTimestamped = %s, want the newest STAMPED event %s; an unstamped newest event must not decide staleness", got, stamped)
	}

	// All unstamped: no conclusion is available, and the caller must treat that
	// as "cannot determine" rather than "idle forever".
	if got := newestTimestamped([]journal.Event{{Type: journal.EventAgentLifecycle}}); !got.IsZero() {
		t.Fatalf("newestTimestamped = %s for wholly unstamped events, want the zero time", got)
	}

	// A normally stamped tail still wins, so the watchdog keeps working.
	newest := stamped.Add(time.Hour)
	events = append(events, journal.Event{Type: journal.EventStageFinished, Time: newest})
	if got := newestTimestamped(events); !got.Equal(newest) {
		t.Fatalf("newestTimestamped = %s, want %s", got, newest)
	}
}

// The behaviour, end to end through EscalateStalled: a run whose newest event
// carries no timestamp, but whose stamped events are RECENT, must not be
// escalated.
//
// This is the cluster case reproduced (#3774): run 8238995d started at 02:54,
// emitted agent.lifecycle with time 0001-01-01T00:00:00Z, and was escalated at
// 03:05 — eleven minutes into a sixty-minute stage, against a forty-five minute
// timeout, because the unstamped event was read as "last active in year 1".
func TestEscalateStalledIgnoresAnUnstampedNewestEvent(t *testing.T) {
	now := time.Date(2026, 8, 28, 3, 5, 0, 0, time.UTC)
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}

	// Stamped events land ELEVEN MINUTES ago — comfortably inside a 45m
	// timeout, so a correct watchdog leaves this run alone.
	clock := now.Add(-11 * time.Minute)
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "agentic-run", Workflow: "impl-real-probe", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement-on-pod", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	// Then the agent starts and its lifecycle event lands UNSTAMPED, which is
	// what the writer actually did.
	clock = time.Time{}
	started := now.Add(-11 * time.Minute)
	if err := run.Append(journal.Event{
		Type: journal.EventAgentLifecycle, Stage: "implement-on-pod", Attempt: 1,
		Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "copilot-1",
			RunID: "agentic-run", Stage: "implement-on-pod", Attempt: 1,
			Lifecycle: journal.AgentStarted, StartedAt: started, UpdatedAt: started,
			Fidelity: journal.AgentFidelityFull,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := New(Config{Worktrees: manager, RunsDir: runsDir})
	if err != nil {
		t.Fatal(err)
	}
	result, escalated, err := r.EscalateStalled("agentic-run", now, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if escalated {
		t.Fatal("escalated a run whose stamped activity was 11 minutes ago against a 45m timeout; an unstamped newest event must not decide staleness, and no timeout can prevent it if it does")
	}
	if result.Phase != journal.PhaseRunning {
		t.Fatalf("phase = %s, want running", result.Phase)
	}
}
