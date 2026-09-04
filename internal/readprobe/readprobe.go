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
	journalOpens        atomic.Uint64
	activeScanDirs      atomic.Uint64
	activeScanOpens     atomic.Uint64
	instanceLogAppends  atomic.Uint64
	instanceLogBytes    atomic.Uint64
	instanceTailReads   atomic.Uint64
	instanceTailBytes   atomic.Uint64
	instanceTailRecords atomic.Uint64
	runPhaseBytes       atomic.Uint64
)

// Snapshot is a point-in-time reading of the counters.
type Snapshot struct {
	// JournalOpens counts run journals opened and parsed by a read path. The
	// §14.2 target for any bounded list page is zero.
	JournalOpens uint64 `json:"journalOpens"`
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
	// InstanceLogAppends counts instance-journal appends, and InstanceLogBytes
	// the bytes those appends READ to allocate their sequence.
	//
	// §2.2's defect measured directly: InstanceLog.Append takes the cross-process
	// journal lock and re-reads the entire journal — 1.30 s at 324 MB, growing
	// without bound, from a write path that shares the process with every read.
	// §14.11 asks for "a bytes-read-per-append bound" as one of the gates on
	// #1914, and this is the counter that bound is asserted against: today
	// InstanceLogBytes/InstanceLogAppends is the whole journal, and afterwards it
	// must be the byte budget.
	InstanceLogAppends uint64 `json:"instanceLogAppends"`
	InstanceLogBytes   uint64 `json:"instanceLogBytesRead"`
	// InstanceTailReads counts incremental instance-journal reads, and
	// InstanceTailBytes the bytes read after the remembered cursor.
	InstanceTailReads uint64 `json:"instanceTailReads"`
	InstanceTailBytes uint64 `json:"instanceTailBytesRead"`
	// InstanceTailRecords counts event records parsed by instance-journal reads.
	InstanceTailRecords uint64 `json:"instanceTailRecordsParsed"`
	// RunPhaseBytes counts the journal bytes read to reconstruct run phases,
	// by whichever route the caller took.
	//
	// It is the counter behind #2755: the daemon's boot reconciliation used to
	// read every byte of every run journal ever written to find the handful
	// still running, and "opened a journal" alone cannot tell that apart from
	// reading its last kilobyte. Opens stay flat either way — only bytes move.
	RunPhaseBytes uint64 `json:"runPhaseBytes"`
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
	activeScanDirs.Store(0)
	activeScanOpens.Store(0)
	instanceLogAppends.Store(0)
	instanceLogBytes.Store(0)
	instanceTailReads.Store(0)
	instanceTailBytes.Store(0)
	instanceTailRecords.Store(0)
	runPhaseBytes.Store(0)
}

// Take returns the current counter values.
func Take() Snapshot {
	return Snapshot{
		JournalOpens:        journalOpens.Load(),
		ActiveScanDirs:      activeScanDirs.Load(),
		ActiveScanOpens:     activeScanOpens.Load(),
		InstanceLogAppends:  instanceLogAppends.Load(),
		InstanceLogBytes:    instanceLogBytes.Load(),
		InstanceTailReads:   instanceTailReads.Load(),
		InstanceTailBytes:   instanceTailBytes.Load(),
		InstanceTailRecords: instanceTailRecords.Load(),
		RunPhaseBytes:       runPhaseBytes.Load(),
	}
}

// Sub returns the work done between an earlier snapshot and this one, so a
// caller can measure one operation without resetting shared state that another
// goroutine may be recording into.
func (s Snapshot) Sub(earlier Snapshot) Snapshot {
	return Snapshot{
		JournalOpens:        s.JournalOpens - earlier.JournalOpens,
		ActiveScanDirs:      s.ActiveScanDirs - earlier.ActiveScanDirs,
		ActiveScanOpens:     s.ActiveScanOpens - earlier.ActiveScanOpens,
		InstanceLogAppends:  s.InstanceLogAppends - earlier.InstanceLogAppends,
		InstanceLogBytes:    s.InstanceLogBytes - earlier.InstanceLogBytes,
		InstanceTailReads:   s.InstanceTailReads - earlier.InstanceTailReads,
		InstanceTailBytes:   s.InstanceTailBytes - earlier.InstanceTailBytes,
		InstanceTailRecords: s.InstanceTailRecords - earlier.InstanceTailRecords,
		RunPhaseBytes:       s.RunPhaseBytes - earlier.RunPhaseBytes,
	}
}

// Zero reports whether every counter in the snapshot is zero — the shape most
// §14 assertions take ("this bounded page did no journal work at all").
func (s Snapshot) Zero() bool { return s == Snapshot{} }

// Enabled reports whether recording is on. Instrumentation that costs more
// than an atomic add — a stat, an allocation — must gate on this so the
// production path keeps paying only the one relaxed load the package promises.
func Enabled() bool { return enabled.Load() }

// RecordRunPhaseBytes records journal bytes read to reconstruct a run's phase.
func RecordRunPhaseBytes(bytesRead int) {
	if bytesRead > 0 && enabled.Load() {
		runPhaseBytes.Add(uint64(bytesRead))
	}
}

// RecordJournalOpen records one run journal opened by a read path.
func RecordJournalOpen() {
	if enabled.Load() {
		journalOpens.Add(1)
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

// RecordInstanceLogAppend records one instance-journal append and the number of
// bytes it read to allocate its sequence.
func RecordInstanceLogAppend(bytesRead int) {
	if enabled.Load() {
		instanceLogAppends.Add(1)
		if bytesRead > 0 {
			instanceLogBytes.Add(uint64(bytesRead))
		}
	}
}

// RecordInstanceTailRead records one incremental instance-journal read.
func RecordInstanceTailRead(bytesRead int) {
	if enabled.Load() {
		instanceTailReads.Add(1)
		if bytesRead > 0 {
			instanceTailBytes.Add(uint64(bytesRead))
		}
	}
}

// RecordInstanceTailRecords records event records parsed by an instance-journal read.
func RecordInstanceTailRecords(records int) {
	if records > 0 && enabled.Load() {
		instanceTailRecords.Add(uint64(records))
	}
}
