package stallsweep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func dueFor(t *testing.T, started, now time.Time, events []journal.Event, maxDuration time.Duration) (string, bool, error) {
	t.Helper()
	sweeper := &StallSweeper{config: StallConfig{
		StalledTimeout: time.Hour, MaxRunDuration: maxDuration,
		Events: func(context.Context, string) ([]journal.Event, error) { return events, nil },
	}}
	reason, _, due, err := sweeper.due(t.Context(), StallActivity{
		Identity:     journal.RunIdentity{RunID: "run", StartedAt: started},
		LastAppendAt: now.Add(-time.Minute), DatabaseNow: now,
	})
	return reason, due, err
}

// A writer clock that is skewed by more than the tolerance must not inflate a
// run's elapsed time into an escalation: the history is reported instead.
func TestMaxDurationRefusesFarSkewedEventClocks(t *testing.T) {
	now := time.Now().UTC()
	started := now.Add(-30 * time.Minute) // well inside a 1h budget
	for _, test := range []struct {
		name   string
		events []journal.Event
		want   string
	}{
		{"far-ahead event", []journal.Event{{Type: journal.EventRunStarted, Time: started}, {Type: journal.EventRunnerAnnotation, Time: now.Add(10 * time.Hour)}}, "ahead of the database"},
		{"far-behind first event", []journal.Event{{Type: journal.EventRunStarted, Time: started.Add(-3 * time.Hour)}}, "before the run start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// pastDuration needs the DB-clock age past the budget, so give it an
			// older start with the same skewed events.
			old := now.Add(-2 * time.Hour)
			_, due, err := dueFor(t, old, now, test.events, time.Hour)
			if due || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("due=%v err=%v, want a %q refusal and no escalation", due, err, test.want)
			}
		})
	}
	// Within the tolerance the history is evaluated normally.
	events := []journal.Event{{Type: journal.EventRunStarted, Time: now.Add(-2 * time.Hour)}, {Type: journal.EventRunnerAnnotation, Time: now.Add(time.Minute)}}
	reason, due, err := dueFor(t, now.Add(-2*time.Hour), now, events, time.Hour)
	if err != nil || !due || reason != "max_duration" {
		t.Fatalf("mild skew: reason=%q due=%v err=%v", reason, due, err)
	}
}
