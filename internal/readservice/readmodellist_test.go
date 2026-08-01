package readservice

import "testing"

// TestDisableReadModelReadsForcesJournalPath is #2036's rollback test: before
// this issue, DisableReadModelReads had zero callers anywhere (not even a
// test), so the design's §6.6 promise — "rollback is a flag flip, never a
// deploy" — was API surface only. This pins that the toggle actually flips
// what readModelEligible answers, with a real ReadModel attached so the
// difference is provably the flag, not the store's absence.
func TestDisableReadModelReadsForcesJournalPath(t *testing.T) {
	service := &Local{sources: LocalSources{ReadModel: brokenReader{}}}
	options := RunListOptions{Limit: 50}

	service.EnableReadModelReads()
	if !service.readModelEligible(options) {
		t.Fatal("readModelEligible() = false with reads enabled and a store attached, want true")
	}

	service.DisableReadModelReads()
	if service.readModelEligible(options) {
		t.Fatal("readModelEligible() = true after DisableReadModelReads(), want false — " +
			"the rollback must force every list request onto the journal-derived paths")
	}
}
