package readprobe

import "testing"

// TestRecordersAreNoOpsWhenDisabled is readprobe's core production
// invariant: every Record* call site pays its atomic load unconditionally,
// so a caller that never Enables must see it stay exactly zero (#2045) —
// this is what makes the seam safe to leave on hot paths in production,
// where it is always disabled.
func TestRecordersAreNoOpsWhenDisabled(t *testing.T) {
	Disable()
	Reset()
	RecordJournalOpen()
	RecordActiveScanDir()
	RecordActiveScanOpen()
	RecordInstanceLogAppend(100)
	RecordInstanceTailRead(100)

	if got := Take(); got != (Snapshot{}) {
		t.Fatalf("Take() = %+v, want all-zero while disabled", got)
	}
}

// TestEnableResetsAndStartsRecording pins the exact counter each Record*
// function feeds, and that Enable zeroes stale counts from a prior
// measurement before recording resumes — the scale gate depends on both:
// a miscounting probe (wrong counter, or one that keeps counting after a
// prior run) silently turns the gate into a no-op (#2045).
func TestEnableResetsAndStartsRecording(t *testing.T) {
	Disable()
	Reset()
	Enable()
	t.Cleanup(Disable)

	RecordJournalOpen()
	RecordJournalOpen()
	RecordActiveScanDir()
	RecordActiveScanDir()
	RecordActiveScanDir()
	RecordActiveScanOpen()
	RecordInstanceLogAppend(64)
	RecordInstanceLogAppend(36)
	RecordInstanceTailRead(25)

	want := Snapshot{
		JournalOpens:       2,
		ActiveScanDirs:     3,
		ActiveScanOpens:    1,
		InstanceLogAppends: 2,
		InstanceLogBytes:   100,
		InstanceTailReads:  1,
		InstanceTailBytes:  25,
	}
	if got := Take(); got != want {
		t.Fatalf("Take() = %+v, want %+v", got, want)
	}

	// Enable again (as a caller measuring a second, independent operation
	// would) must zero the prior measurement's counts, not accumulate them.
	Enable()
	if got := Take(); got != (Snapshot{}) {
		t.Fatalf("Take() after re-Enable = %+v, want all-zero (Enable must Reset)", got)
	}
}

// TestRecordInstanceLogAppendOnlyAddsPositiveBytes: the counter's own
// RecordInstanceLogAppend contract is "if bytesRead > 0" — a zero or
// negative bytesRead still counts the append but must not perturb the byte
// total, since §14.11's bound is InstanceLogBytes/InstanceLogAppends and a
// spurious negative or zero addend would silently understate it.
func TestRecordInstanceLogAppendOnlyAddsPositiveBytes(t *testing.T) {
	Disable()
	Reset()
	Enable()
	t.Cleanup(Disable)

	RecordInstanceLogAppend(0)
	RecordInstanceLogAppend(-5)
	RecordInstanceLogAppend(10)

	want := Snapshot{InstanceLogAppends: 3, InstanceLogBytes: 10}
	if got := Take(); got != want {
		t.Fatalf("Take() = %+v, want %+v", got, want)
	}
}

// TestDisableStopsRecordingButKeepsCounters: Disable turns recording off
// without clearing what was already measured, so a caller can stop and then
// read — distinct from Reset, which zeroes explicitly.
func TestDisableStopsRecordingButKeepsCounters(t *testing.T) {
	Disable()
	Reset()
	Enable()
	RecordJournalOpen()
	Disable()

	if got := Take(); got.JournalOpens != 1 {
		t.Fatalf("JournalOpens after Disable = %d, want 1 (Disable must not clear counters)", got.JournalOpens)
	}

	// And recording has genuinely stopped: a call after Disable must not
	// increment further.
	RecordJournalOpen()
	if got := Take(); got.JournalOpens != 1 {
		t.Fatalf("JournalOpens after a post-Disable call = %d, want still 1", got.JournalOpens)
	}
}

// TestSubComputesDelta is the seam a caller measuring one operation amid
// concurrent unrelated recording relies on instead of Reset (per the
// package doc's concurrency note) — it must be a plain per-field
// subtraction, not e.g. accidentally swapped operand order.
func TestSubComputesDelta(t *testing.T) {
	earlier := Snapshot{JournalOpens: 5, ActiveScanDirs: 10, ActiveScanOpens: 2, InstanceLogAppends: 1, InstanceLogBytes: 50}
	later := Snapshot{JournalOpens: 8, ActiveScanDirs: 10, ActiveScanOpens: 3, InstanceLogAppends: 4, InstanceLogBytes: 150}

	want := Snapshot{JournalOpens: 3, ActiveScanDirs: 0, ActiveScanOpens: 1, InstanceLogAppends: 3, InstanceLogBytes: 100}
	if got := later.Sub(earlier); got != want {
		t.Fatalf("Sub() = %+v, want %+v", got, want)
	}
}

// TestZero pins the exact shape most §14 assertions take: "this bounded
// page did no journal work at all."
func TestZero(t *testing.T) {
	if !(Snapshot{}).Zero() {
		t.Fatal("zero-value Snapshot.Zero() = false, want true")
	}
	if (Snapshot{JournalOpens: 1}).Zero() {
		t.Fatal("Snapshot with JournalOpens=1 .Zero() = true, want false")
	}
	if (Snapshot{InstanceLogBytes: 1}).Zero() {
		t.Fatal("Snapshot with InstanceLogBytes=1 .Zero() = true, want false")
	}
}
