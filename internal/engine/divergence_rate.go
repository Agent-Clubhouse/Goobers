package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DivergenceRateWindow is the period the ceiling below is measured over.
const DivergenceRateWindow = time.Hour

// DefaultDivergenceRateCeiling is how many DS5 divergences may be filed
// verbatim per window before the reporter switches to aggregating.
//
// A parity guard's value is that a filing MEANS something. #4244 measured
// 10,220 live_journal_divergence events in ten days — 10,218 of 13,619 error
// events in the window — at which point the channel no longer reports drift,
// it IS the drift. The ceiling is set well above any plausible genuine rate
// (the residue after verify.go's asymmetry fix is expected to be single
// digits per day) and well below a saturating one, so it never truncates real
// signal and always converts a storm into one countable alarm.
const DefaultDivergenceRateCeiling = 20

// DivergenceRateLimiter wraps a DivergenceReporter with a per-window ceiling,
// so saturation becomes the alarm instead of drowning it.
//
// Under the ceiling every divergence is filed verbatim — a guard that
// summarizes its first finding is useless. On crossing it, one ceiling event
// is filed immediately (so the operator learns of the storm at the moment it
// starts, not an hour later), and subsequent divergences are counted by
// signature instead of filed. Flush emits one aggregate naming the suppressed
// count and the top signatures.
type DivergenceRateLimiter struct {
	inner   DivergenceReporter
	ceiling int
	window  time.Duration
	now     func() time.Time

	mu           sync.Mutex
	windowStart  time.Time
	filed        int
	suppressed   int
	signatures   map[string]int
	suppressedID string
}

// NewDivergenceRateLimiter wraps inner. A nil inner yields nil, so a caller
// with no reporter wired stays exactly as it was. A non-positive ceiling or
// window falls back to the defaults.
func NewDivergenceRateLimiter(inner DivergenceReporter, ceiling int, window time.Duration, now func() time.Time) *DivergenceRateLimiter {
	if inner == nil {
		return nil
	}
	if ceiling <= 0 {
		ceiling = DefaultDivergenceRateCeiling
	}
	if window <= 0 {
		window = DivergenceRateWindow
	}
	if now == nil {
		now = time.Now
	}
	return &DivergenceRateLimiter{inner: inner, ceiling: ceiling, window: window, now: now}
}

// Report is the DivergenceReporter the reconciler is given. A nil receiver
// reports nothing, which is what NewDivergenceRateLimiter's nil-inner case
// means — so the method value can be handed over unconditionally.
func (l *DivergenceRateLimiter) Report(runID, detail string) {
	if l == nil {
		return
	}
	// Decide under the lock, emit outside it: inner appends to the instance
	// journal, and holding the lock across that would serialize the reconcile
	// pass behind a disk write.
	var pending []func()
	l.mu.Lock()
	now := l.now()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= l.window {
		pending = append(pending, l.rollLocked(now)...)
	}
	l.filed++
	switch {
	case l.filed <= l.ceiling:
		runID, detail := runID, detail
		pending = append(pending, func() { l.inner(runID, detail) })
	default:
		l.recordSuppressedLocked(runID, detail)
		if l.filed == l.ceiling+1 {
			id, msg := l.suppressedID, fmt.Sprintf(
				"divergence rate ceiling reached: %d filings within %s; further divergences this window are counted, not filed — see the aggregate that follows",
				l.filed, l.window)
			pending = append(pending, func() { l.inner(id, msg) })
		}
	}
	l.mu.Unlock()
	for _, emit := range pending {
		emit()
	}
}

// Flush files the aggregate for whatever has been suppressed so far and
// clears it, WITHOUT reopening the window's budget — the ceiling stays in
// force for the rest of the window. The daemon calls it at the end of each
// reconcile pass so a storm is summarized while it is still news, rather than
// waiting for the window to roll.
func (l *DivergenceRateLimiter) Flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	pending := l.drainLocked()
	l.mu.Unlock()
	for _, emit := range pending {
		emit()
	}
}

// rollLocked closes the previous window and starts one at now.
func (l *DivergenceRateLimiter) rollLocked(now time.Time) []func() {
	pending := l.drainLocked()
	l.windowStart = now
	l.filed = 0
	return pending
}

// drainLocked turns any accumulated suppressions into a single aggregate
// emission and resets the accumulator. It deliberately does not touch filed
// or windowStart; see Flush.
func (l *DivergenceRateLimiter) drainLocked() []func() {
	if l.suppressed == 0 {
		return nil
	}
	id, msg := l.suppressedID, fmt.Sprintf(
		"aggregated %d suppressed divergences over a %s ceiling of %d; top signatures: %s",
		l.suppressed, l.window, l.ceiling, formatSignatures(l.signatures))
	l.suppressed = 0
	l.signatures = nil
	l.suppressedID = ""
	return []func(){func() { l.inner(id, msg) }}
}

func (l *DivergenceRateLimiter) recordSuppressedLocked(runID, detail string) {
	if l.signatures == nil {
		l.signatures = map[string]int{}
	}
	l.suppressed++
	l.signatures[divergenceSignature(detail)]++
	if l.suppressedID == "" {
		// Name a real run so the aggregate is traceable to at least one
		// instance of what it is summarizing.
		l.suppressedID = runID
	}
}

// maxAggregatedSignatures bounds the aggregate's signature list, so the
// summary of a bounded-size channel cannot itself be unbounded.
const maxAggregatedSignatures = 5

func formatSignatures(counts map[string]int) string {
	if len(counts) == 0 {
		return "(none)"
	}
	type entry struct {
		signature string
		count     int
	}
	entries := make([]entry, 0, len(counts))
	for signature, count := range counts {
		entries = append(entries, entry{signature, count})
	}
	// Count descending, then signature ascending — a stable order, so the
	// same storm always reads the same way.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].signature < entries[j].signature
	})
	shown := entries
	remainder := 0
	if len(shown) > maxAggregatedSignatures {
		remainder = len(shown) - maxAggregatedSignatures
		shown = shown[:maxAggregatedSignatures]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, e := range shown {
		parts = append(parts, fmt.Sprintf("%s ×%d", e.signature, e.count))
	}
	if remainder > 0 {
		parts = append(parts, fmt.Sprintf("(+%d more)", remainder))
	}
	return strings.Join(parts, ", ")
}

// divergenceSignature reduces one divergence detail to the shape it belongs
// to, discarding the per-run particulars (indexes, artifact names, counts) so
// that ten thousand instances of one bug aggregate into one line. It reads
// the details diffNormativeViews and verifyLiveRun produce, and falls back to
// the leading line for anything else.
func divergenceSignature(detail string) string {
	var live, projected string
	lengths := strings.HasPrefix(detail, "normative view lengths diverge")
	for _, line := range strings.Split(detail, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "live:"):
			live = eventTypeToken(trimmed)
		case strings.HasPrefix(trimmed, "projected:"):
			projected = eventTypeToken(trimmed)
		case lengths:
			if token := eventTypeToken(trimmed); token != "" {
				live = token
			}
		}
	}
	switch {
	case lengths && live != "":
		return "length-mismatch extra=" + live
	case live != "" || projected != "":
		return fmt.Sprintf("live=%s projected=%s", orUnknown(live), orUnknown(projected))
	}
	return firstLine(detail)
}

func eventTypeToken(line string) string {
	_, rest, found := strings.Cut(line, "type=")
	if !found {
		return ""
	}
	token, _, _ := strings.Cut(rest, " ")
	return token
}

func orUnknown(s string) string {
	if s == "" {
		return "(absent)"
	}
	return s
}

// maxSignatureLine bounds an unrecognized detail's fallback signature, so a
// novel shape cannot make the aggregate as large as the storm it summarizes.
const maxSignatureLine = 120

func firstLine(detail string) string {
	line, _, _ := strings.Cut(detail, "\n")
	line = strings.TrimSpace(line)
	if len(line) > maxSignatureLine {
		return line[:maxSignatureLine] + "…"
	}
	if line == "" {
		return "(empty)"
	}
	return line
}
