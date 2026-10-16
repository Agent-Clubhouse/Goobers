package hostsuspend

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

var base = time.Date(2026, 10, 2, 17, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

func TestOverlapClipsAndMergesWindows(t *testing.T) {
	windows := []Window{
		{From: at(60), To: at(120)},
		{From: at(0), To: at(30)},
		{From: at(90), To: at(150)},  // overlaps the first: counted once
		{From: at(200), To: at(190)}, // inverted: ignored
	}
	for _, tc := range []struct {
		name     string
		from, to time.Time
		want     time.Duration
	}{
		{"all", at(-10), at(300), 120 * time.Minute},
		{"clipped both ends", at(10), at(100), 60 * time.Minute},
		{"between windows", at(30), at(60), 0},
		{"empty range", at(100), at(100), 0},
		{"inverted range", at(100), at(10), 0},
	} {
		if got := Overlap(windows, tc.from, tc.to); got != tc.want {
			t.Errorf("%s: Overlap = %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := Overlap(nil, at(0), at(10)); got != 0 {
		t.Fatalf("Overlap(nil) = %s, want 0", got)
	}
}

type fakeClock struct{ suspended time.Duration }

func (c fakeClock) SuspendedSince() time.Duration { return c.suspended }

func TestLedgerJournalsObservedSuspensionForLaterLifetimes(t *testing.T) {
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	earlier := Window{From: at(-600), To: at(-540)}
	next := []time.Duration{0, 3 * time.Hour, 0}
	ledger := newLedger(log, []Window{earlier}, func() suspendClock {
		clock := fakeClock{suspended: next[0]}
		next = next[1:]
		return clock
	})

	// The first mark saw nothing; the second reports the three-hour sleep
	// that ended when the sweep at 20:00 ran.
	if err := ledger.Observe(at(0)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Observe(at(180)); err != nil {
		t.Fatal(err)
	}
	want := []Window{earlier, {From: at(0), To: at(180)}}
	assertWindows(t, ledger.Windows(), want)

	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatal(err)
	}
	assertWindows(t, FromEvents(events), want[1:])
}

func TestFromEventsIgnoresMalformedRecords(t *testing.T) {
	valid := suspensionEvent(Window{From: at(0), To: at(10)})
	inverted := suspensionEvent(Window{From: at(10), To: at(0)})
	otherKind := suspensionEvent(Window{From: at(0), To: at(10)})
	otherKind.Runner = map[string]any{"kind": journal.RunnerAnnotationRunRecovery, payloadFrom: valid.Runner[payloadFrom], payloadTo: valid.Runner[payloadTo]}
	unparsable := suspensionEvent(Window{From: at(0), To: at(10)})
	unparsable.Runner = map[string]any{"kind": journal.RunnerAnnotationHostSuspended, payloadFrom: "yesterday", payloadTo: valid.Runner[payloadTo]}
	assertWindows(t, FromEvents([]journal.Event{valid, inverted, otherKind, unparsable}), []Window{{From: at(0), To: at(10)}})
}

func TestNilLedgerObservesNothing(t *testing.T) {
	var ledger *Ledger
	if err := ledger.Observe(at(0)); err != nil || ledger.Windows() != nil {
		t.Fatalf("nil ledger = %v, %v", err, ledger.Windows())
	}
}

func assertWindows(t *testing.T, got, want []Window) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].From.Equal(want[i].From) || !got[i].To.Equal(want[i].To) {
			t.Fatalf("windows = %+v, want %+v", got, want)
		}
	}
}
