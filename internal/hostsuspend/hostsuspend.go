// Package hostsuspend records the intervals the host spent suspended while
// the daemon was running, so budgets measured between persisted wall-clock
// journal timestamps — runControls.maxRunDuration and the stalled-run sweep —
// can exclude time in which nothing could execute (#5891).
//
// The daemon observes suspension between its own periodic sweeps through
// activetime.WallMark and journals each interval to the instance log, so a
// later daemon lifetime still credits it. Suspension while no daemon is
// running is not observable here and keeps counting, as daemon downtime does.
package hostsuspend

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/activetime"
)

const (
	payloadFrom = "suspendedFrom"
	payloadTo   = "suspendedTo"
)

// Window is one closed interval of wall-clock time.
type Window struct {
	From, To time.Time
}

// Overlap reports how much of [from, to] the union of windows covers, so
// intervals that overlap one another are never subtracted twice.
func Overlap(windows []Window, from, to time.Time) time.Duration {
	if !to.After(from) || len(windows) == 0 {
		return 0
	}
	clipped := make([]Window, 0, len(windows))
	for _, window := range windows {
		start, end := window.From, window.To
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if end.After(start) {
			clipped = append(clipped, Window{From: start, To: end})
		}
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].From.Before(clipped[j].From) })
	var total time.Duration
	var current Window
	for i, window := range clipped {
		if i > 0 && !window.From.After(current.To) {
			if window.To.After(current.To) {
				current.To = window.To
			}
			continue
		}
		total += current.To.Sub(current.From)
		current = window
	}
	return total + current.To.Sub(current.From)
}

// FromEvents returns every host suspension journaled in events. Records
// without a valid, positive interval are ignored rather than guessed at.
func FromEvents(events []journal.Event) []Window {
	var windows []Window
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != journal.RunnerAnnotationHostSuspended {
			continue
		}
		from, fromErr := parseTime(event.Runner[payloadFrom])
		to, toErr := parseTime(event.Runner[payloadTo])
		if fromErr != nil || toErr != nil || !to.After(from) {
			continue
		}
		windows = append(windows, Window{From: from, To: to})
	}
	return windows
}

func parseTime(value any) (time.Time, error) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("not a timestamp: %v", value)
	}
	return time.Parse(time.RFC3339Nano, text)
}

func suspensionEvent(window Window) journal.Event {
	return journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Reason: fmt.Sprintf("host suspended for %s", window.To.Sub(window.From).Round(time.Second)),
		Runner: map[string]any{
			"kind":      journal.RunnerAnnotationHostSuspended,
			payloadFrom: window.From.UTC().Format(time.RFC3339Nano),
			payloadTo:   window.To.UTC().Format(time.RFC3339Nano),
		},
	}
}

type suspendClock interface {
	SuspendedSince() time.Duration
}

// Ledger is the daemon's record of host suspension: what earlier lifetimes
// journaled plus what Observe has seen in this one.
type Ledger struct {
	mu      sync.Mutex
	log     *journal.InstanceLog
	newMark func() suspendClock
	mark    suspendClock
	windows []Window
}

// NewLedger starts observing from now. recorded seeds the intervals earlier
// daemon lifetimes journaled (see FromEvents); log, when non-nil, receives
// each interval Observe finds.
func NewLedger(log *journal.InstanceLog, recorded []Window) *Ledger {
	return newLedger(log, recorded, func() suspendClock { return activetime.NewWallMark() })
}

func newLedger(log *journal.InstanceLog, recorded []Window, newMark func() suspendClock) *Ledger {
	return &Ledger{
		log:     log,
		newMark: newMark,
		mark:    newMark(),
		windows: append([]Window(nil), recorded...),
	}
}

// Observe records any suspension since the previous Observe (or NewLedger)
// as the interval ending at now. Where inside that span the host actually
// slept is unknown, so callers observing at a fixed cadence bound the
// placement error by that cadence. The interval is retained even when
// journaling it fails; the error is returned for reporting.
func (l *Ledger) Observe(now time.Time) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	suspended := l.mark.SuspendedSince()
	l.mark = l.newMark()
	if suspended <= 0 {
		return nil
	}
	to := now.Round(0).UTC()
	window := Window{From: to.Add(-suspended), To: to}
	l.windows = append(l.windows, window)
	if l.log == nil {
		return nil
	}
	if err := l.log.Append(suspensionEvent(window)); err != nil {
		return fmt.Errorf("journal host suspension: %w", err)
	}
	return nil
}

// Windows returns a snapshot of every known suspension interval.
func (l *Ledger) Windows() []Window {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Window(nil), l.windows...)
}
