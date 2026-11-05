package stallsweep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// dueFor evaluates a history stored before the journal recorded its commit
// clock: records carry only the writer's Time.
func dueFor(t *testing.T, started, now time.Time, events []journal.Event, maxDuration time.Duration, report func(error)) (string, bool, error) {
	t.Helper()
	records := make([]journal.EventRecord, len(events))
	for i, event := range events {
		records[i] = journal.EventRecord{Event: event}
	}
	return dueForRecords(t, started, now, records, maxDuration, report)
}

func dueForRecords(t *testing.T, started, now time.Time, records []journal.EventRecord, maxDuration time.Duration, report func(error)) (string, bool, error) {
	t.Helper()
	sweeper := &StallSweeper{config: StallConfig{
		StalledTimeout: time.Hour, MaxRunDuration: maxDuration, Report: report,
		Events: func(context.Context, string) ([]journal.EventRecord, error) { return records, nil },
	}}
	reason, _, due, err := sweeper.due(t.Context(), StallActivity{
		Identity:     journal.RunIdentity{RunID: "run", StartedAt: started},
		LastAppendAt: now.Add(-time.Minute), DatabaseNow: now,
	})
	return reason, due, err
}

// A writer clock skewed past the tolerance must not buy a run extra time: the
// child waits get no credit, the run is measured by its raw database-clock age,
// and the finding is reported. A return stamped a year ahead would otherwise
// stretch both "now" and the subtracted wait and evade the budget forever.
func TestMaxDurationFailsClosedOnFarSkewedEventClocks(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour) // database-clock age 2h against a 1h budget
	for _, test := range []struct {
		name   string
		events []journal.Event
		want   string
	}{
		{"far-ahead event", []journal.Event{{Type: journal.EventRunStarted, Time: old}, {Type: journal.EventRunnerAnnotation, Time: now.Add(365 * 24 * time.Hour)}}, "ahead of the database"},
		{"far-behind first event", []journal.Event{{Type: journal.EventRunStarted, Time: old.Add(-3 * time.Hour)}}, "before the run start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reported []error
			reason, due, err := dueFor(t, old, now, test.events, time.Hour, func(err error) { reported = append(reported, err) })
			if err != nil || !due || reason != "max_duration" {
				t.Fatalf("reason=%q due=%v err=%v, want max_duration (fail closed)", reason, due, err)
			}
			if len(reported) != 1 || !strings.Contains(reported[0].Error(), test.want) {
				t.Fatalf("reported = %v, want one %q finding", reported, test.want)
			}
		})
	}
	// Within the tolerance the history is evaluated normally, with no report.
	events := []journal.Event{{Type: journal.EventRunStarted, Time: old}, {Type: journal.EventRunnerAnnotation, Time: now.Add(time.Minute)}}
	var reported []error
	reason, due, err := dueFor(t, old, now, events, time.Hour, func(err error) { reported = append(reported, err) })
	if err != nil || !due || reason != "max_duration" || len(reported) != 0 {
		t.Fatalf("mild skew: reason=%q due=%v err=%v reported=%v", reason, due, err, reported)
	}
	// An honest 30m run is still not due.
	reason, due, err = dueFor(t, now.Add(-30*time.Minute), now,
		[]journal.Event{{Type: journal.EventRunStarted, Time: now.Add(-30 * time.Minute)}}, time.Hour, nil)
	if err != nil || due {
		t.Fatalf("honest run: reason=%q due=%v err=%v", reason, due, err)
	}
}

// With the journal's commit clock on every record, writer clocks are never
// consulted: a writer skewed by a year produces no finding and no fail-closed
// verdict, and an honest 30m run stays inside its 1h budget.
func TestRecordedClockReplacesWriterClock(t *testing.T) {
	now := time.Now().UTC()
	started := now.Add(-30 * time.Minute)
	for name, skew := range map[string]time.Duration{"ahead": 365 * 24 * time.Hour, "behind": -365 * 24 * time.Hour} {
		records := []journal.EventRecord{
			{Event: journal.Event{Type: journal.EventRunStarted, Time: started.Add(skew)}, RecordedAt: started},
			{Event: journal.Event{Type: journal.EventRunnerAnnotation, Time: now.Add(skew)}, RecordedAt: now.Add(-time.Minute)},
		}
		var reported []error
		reason, due, err := dueForRecords(t, started, now, records, time.Hour, func(err error) { reported = append(reported, err) })
		if err != nil || due || len(reported) != 0 {
			t.Fatalf("%s: reason=%q due=%v err=%v reported=%v, want an honest verdict with no skew finding", name, reason, due, err, reported)
		}
	}
	// One record without the commit clock sends the whole history to the
	// writer-clock path, where the year of skew fails closed.
	mixed := []journal.EventRecord{
		{Event: journal.Event{Type: journal.EventRunStarted, Time: started}, RecordedAt: started},
		{Event: journal.Event{Type: journal.EventRunnerAnnotation, Time: now.Add(365 * 24 * time.Hour)}},
	}
	var reported []error
	if _, _, err := dueForRecords(t, now.Add(-2*time.Hour), now, mixed, time.Hour, func(err error) { reported = append(reported, err) }); err != nil || len(reported) != 1 {
		t.Fatalf("mixed history: err=%v reported=%v, want the writer-clock skew finding", err, reported)
	}
}
