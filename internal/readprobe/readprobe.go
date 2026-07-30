// Package readprobe counts the expensive work a read path performs, so tests
// and the load harness can assert on *work done* rather than only on wall time.
//
// # Why this exists
//
// The design's acceptance bar is stated in work, not latency: a `bounded` route
// must open zero run journals and read zero directories (§14.1, §14.2), and
// "`limit+1` returned rows does not bound rows examined" (§5.7). A latency
// assertion cannot distinguish "fast because it is bounded" from "fast because
// the corpus is small", which is precisely how a read path can regress into
// O(history) without any test noticing — the pattern this whole epic exists to
// end.
//
// The instrumentation the diagnosis recommended reusing (its Appendix B:
// openRunObserver, reconcileScanObserver/reconcileInspectObserver,
// journalReadObserver) already existed, and each is a **package-private
// function variable**. That makes them reachable only from their own package's
// tests — so the cross-cutting property "this HTTP route opened no journals"
// had no way to be asserted, and the load harness (package main) could not see
// them at all.
//
// This package is that missing seam: one process-global counter set the
// existing observers feed, readable from anywhere.
//
// # Cost when disabled
//
// Disabled is the default and the only production state. Each instrumented call
// site pays one relaxed atomic load, which is a few nanoseconds and no
// allocation, so the seam can live on hot paths without a build tag. Nothing is
// recorded and no identity is retained unless a caller explicitly enables it.
//
// # Concurrency
//
// Counters are atomic and safe under concurrent reads. Enable/Reset are not
// synchronized against in-flight recording — a caller enabling the probe while
// requests are already running may miss or double-count the work in flight at
// that instant. That is deliberate: making it exact would need a lock on every
// hot-path call, which is a real cost to buy a guarantee no test needs. Enable
// before issuing the work you intend to measure.
package readprobe

import "sync/atomic"

// enabled gates recording. Read on every instrumented call site, so it stays a
// plain atomic bool rather than anything that could allocate or block.
var enabled atomic.Bool

var (
	journalOpens       atomic.Uint64
	reconcileScans     atomic.Uint64
	reconcileInspects  atomic.Uint64
	streamJournalReads atomic.Uint64
	activeScanDirs     atomic.Uint64
	activeScanOpens    atomic.Uint64
)

// Snapshot is a point-in-time reading of the counters.
type Snapshot struct {
	// JournalOpens counts run journals opened and parsed by a read path. The
	// §14.2 target for any bounded list page is zero.
	JournalOpens uint64 `json:"journalOpens"`
	// ReconcileScans counts request-path reconciliation passes — a read
	// performing maintenance (§2.4). The target is zero once Wave 3 lands.
	ReconcileScans uint64 `json:"reconcileScans"`
	// ReconcileInspects counts directory entries examined by those passes. This
	// is the one that grows with total history rather than with the working set.
	ReconcileInspects uint64 `json:"reconcileInspects"`
	// StreamJournalReads counts journals read by the change detector. Bounded by
	// active work, never by history (#1738).
	StreamJournalReads uint64 `json:"streamJournalReads"`
	// ActiveScanDirs counts directory entries walked by the active-run scan, and
	// ActiveScanOpens the journals it opens and replays to reconstruct phase.
	//
	// These are the numbers behind §2.1's headline — 17.2 s cold to answer "2" —
	// and nothing observed them before. The diagnosis's Appendix B recommended
	// reusing three existing seams, none of which covers this path: the scan runs
	// in localscheduler and opens journals through journal.OpenRead, not through
	// readservice.openRun, so a "zero journal opens" assertion built on the
	// existing seams reports a clean zero for the most expensive read in the
	// system. Measured: the inventory surfaces show 0 opens on those seams while
	// taking 18x a bounded list page.
	ActiveScanDirs  uint64 `json:"activeScanDirs"`
	ActiveScanOpens uint64 `json:"activeScanOpens"`
}

// Enable turns recording on and zeroes the counters, so a caller measuring a
// single operation does not inherit another's work.
func Enable() {
	Reset()
	enabled.Store(true)
}

// Disable turns recording off. Counters keep their values so a caller can stop
// recording and then read.
func Disable() { enabled.Store(false) }

// Reset zeroes every counter without changing the enabled state.
func Reset() {
	journalOpens.Store(0)
	reconcileScans.Store(0)
	reconcileInspects.Store(0)
	streamJournalReads.Store(0)
	activeScanDirs.Store(0)
	activeScanOpens.Store(0)
}

// Take returns the current counter values.
func Take() Snapshot {
	return Snapshot{
		JournalOpens:       journalOpens.Load(),
		ReconcileScans:     reconcileScans.Load(),
		ReconcileInspects:  reconcileInspects.Load(),
		StreamJournalReads: streamJournalReads.Load(),
		ActiveScanDirs:     activeScanDirs.Load(),
		ActiveScanOpens:    activeScanOpens.Load(),
	}
}

// Sub returns the work done between an earlier snapshot and this one, so a
// caller can measure one operation without resetting shared state that another
// goroutine may be recording into.
func (s Snapshot) Sub(earlier Snapshot) Snapshot {
	return Snapshot{
		JournalOpens:       s.JournalOpens - earlier.JournalOpens,
		ReconcileScans:     s.ReconcileScans - earlier.ReconcileScans,
		ReconcileInspects:  s.ReconcileInspects - earlier.ReconcileInspects,
		StreamJournalReads: s.StreamJournalReads - earlier.StreamJournalReads,
		ActiveScanDirs:     s.ActiveScanDirs - earlier.ActiveScanDirs,
		ActiveScanOpens:    s.ActiveScanOpens - earlier.ActiveScanOpens,
	}
}

// Zero reports whether every counter in the snapshot is zero — the shape most
// §14 assertions take ("this bounded page did no journal work at all").
func (s Snapshot) Zero() bool { return s == Snapshot{} }

// RecordJournalOpen records one run journal opened by a read path.
func RecordJournalOpen() {
	if enabled.Load() {
		journalOpens.Add(1)
	}
}

// RecordReconcileScan records one request-path reconciliation pass.
func RecordReconcileScan() {
	if enabled.Load() {
		reconcileScans.Add(1)
	}
}

// RecordReconcileInspect records one directory entry examined during
// reconciliation.
func RecordReconcileInspect() {
	if enabled.Load() {
		reconcileInspects.Add(1)
	}
}

// RecordStreamJournalRead records one journal read by the change detector.
func RecordStreamJournalRead() {
	if enabled.Load() {
		streamJournalReads.Add(1)
	}
}

// RecordActiveScanDir records one directory entry walked by the active-run scan.
func RecordActiveScanDir() {
	if enabled.Load() {
		activeScanDirs.Add(1)
	}
}

// RecordActiveScanOpen records one journal opened and replayed by the
// active-run scan to reconstruct a run's phase.
func RecordActiveScanOpen() {
	if enabled.Load() {
		activeScanOpens.Add(1)
	}
}
