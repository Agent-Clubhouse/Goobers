package readmodel

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestGateRepassProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*journal.Event)
		want   int
	}{
		{"policy", func(e *journal.Event) {}, 1},
		{"infra", func(e *journal.Event) { e.Verdict = "infra" }, 1},
		{"decoded count", func(e *journal.Event) { e.Runner["repassAttempt"] = float64(3) }, 1},
		{"legacy", func(e *journal.Event) { delete(e.Runner, "repassTarget") }, 1},
		{"forward", func(e *journal.Event) {
			e.Target = "first-remediation"
			delete(e.Runner, "repassTarget")
		}, 0},
		{"pass loop", func(e *journal.Event) { e.Verdict = "pass" }, 0},
		{"poll", func(e *journal.Event) {
			e.Verdict = "timeout"
			e.Runner = map[string]any{"pollTarget": "build", "pollAttempt": 3}
		}, 0},
		{"exhausted", func(e *journal.Event) { e.Target = "@escalate" }, 0},
		{"exhausted custom target", func(e *journal.Event) { e.Target = "park" }, 0},
		{"terminal complete", func(e *journal.Event) { e.Target = "" }, 0},
		{"interrupted", func(e *journal.Event) { e.Runner["interrupted"] = true }, 0},
		{"human decision", func(e *journal.Event) { e.Actor = "operator" }, 0},
		{"unknown schema", func(e *journal.Event) { e.Schema = "future" }, 0},
		{"uncharged", func(e *journal.Event) { e.Runner["repassAttempt"] = 0 }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []journal.Event{
				ev(1, time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage = "build" }),
				ev(2, 2*time.Second, journal.EventStageFinished, func(e *journal.Event) { e.Stage, e.Status = "build", "success" }),
				ev(3, 3*time.Second, journal.EventGateEvaluated, func(e *journal.Event) {
					e.Gate, e.Verdict, e.Target = "quality", "needs-changes", "build"
					e.Runner = map[string]any{"repassTarget": "build", "repassAttempt": 3}
					tc.mutate(e)
				}),
			}
			for split := 0; split <= len(events); split++ {
				first := ProjectRun(testIdentity(), Projection{}, events[:split])
				got := ProjectRun(testIdentity(), first, events[split:]).Run.RepassCount
				if got != tc.want {
					t.Errorf("split %d: repasses = %d, want %d", split, got, tc.want)
				}
			}
		})
	}
}

func TestGateRepassProjectionCountsRoutesAcrossGatesAndBranches(t *testing.T) {
	var events []journal.Event
	for i, gate := range []string{"review", "ci", "review"} {
		events = append(events, ev(uint64(i+1), time.Duration(i+1)*time.Second, journal.EventGateEvaluated, func(e *journal.Event) {
			e.Gate, e.Target, e.Verdict = gate, "build", "needs-changes"
			e.Branch = i % 2
			e.Runner = map[string]any{"repassTarget": "build", "repassAttempt": i + 1}
		}))
	}
	// Preserve the existing count for explicit rerun requests. The following
	// human attempt must not add a second count for that same request.
	events = append(events,
		ev(4, 4*time.Second, journal.EventStageRerunRequested, func(e *journal.Event) { e.Stage = "build" }),
		ev(5, 5*time.Second, journal.EventStageStarted, func(e *journal.Event) { e.Stage, e.AttemptClass = "build", journal.AttemptHuman }),
	)
	for split := 0; split <= len(events); split++ {
		first := ProjectRun(testIdentity(), Projection{}, events[:split])
		got := ProjectRun(testIdentity(), first, events[split:]).Run.RepassCount
		if got != 4 {
			t.Errorf("split %d: repasses = %d, want 4 (three routes and one operator request)", split, got)
		}
	}
}

func TestRepassProjectionUpgradeAtSamePosition(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	events := []journal.Event{ev(1, time.Second, journal.EventGateEvaluated, func(e *journal.Event) {
		e.Gate, e.Verdict, e.Target = "quality", "needs-changes", "build"
		e.Runner = map[string]any{"repassTarget": "build", "repassAttempt": 1}
	})}
	corrected := ProjectRun(testIdentity(), Projection{}, events)
	stale := corrected
	stale.Run.RepassCount = 0
	if err := store.UpsertRun(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := store.writer.ExecContext(ctx, "UPDATE run SET projection_version = ?", currentProjectionVersion-1); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRun(ctx, corrected); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetRun(ctx, corrected.Run.RunID)
	if err != nil || !ok || got.RepassCount != 1 {
		t.Fatalf("upgraded row = %+v, found=%v, err=%v", got, ok, err)
	}
	if err := store.UpsertRun(ctx, corrected); err != nil {
		t.Fatal(err)
	}
	changes, err := store.Changes(ctx, 0, 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("upgrade must publish once: changes=%+v err=%v", changes, err)
	}
}
