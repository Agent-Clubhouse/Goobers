package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readservice"
)

// TestRecoveryOverflowHoldsTheExhaustedAlarmUntilItDrains is #5370's alarm
// contract, and the reason overflow had to enter the classification rather
// than sit beside it. Occupancy falls below the cap the instant one slot
// frees, while work is still waiting at the ref tier; classifying on slots
// alone would retract the alarm there and re-fire it on the next capture.
// The alarm therefore fires once on entering overflow and re-arms only after
// the overflow count returns to zero.
func TestRecoveryOverflowHoldsTheExhaustedAlarmUntilItDrains(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	log := openTestInstanceLog(t)
	at := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	sample := func(used, overflow int) *readservice.RecoveryInventoryStatus {
		return &readservice.RecoveryInventoryStatus{
			State:    readservice.ClassifyRecoveryInventory(used, 10, overflow),
			Used:     used,
			Limit:    10,
			Overflow: overflow,
		}
	}

	reportRecoveryInventoryHealth(layout, log, sample(10, 1), at)
	// A slot frees: occupancy is back under the cap, but the ref tier is not
	// empty, so nothing has actually cleared.
	reportRecoveryInventoryHealth(layout, log, sample(9, 1), at)
	reportRecoveryInventoryHealth(layout, log, sample(5, 2), at)
	if got := recoveryInventoryWarnings(t, log); len(got) != 1 {
		t.Fatalf("warning records = %v; want the exhausted record exactly once while overflow persists", got)
	}

	// Promotion drains the tier: the alarm re-arms, and a second episode is
	// reported as a second episode.
	reportRecoveryInventoryHealth(layout, log, sample(5, 0), at)
	reportRecoveryInventoryHealth(layout, log, sample(6, 3), at)
	if got := recoveryInventoryWarnings(t, log); len(got) != 2 {
		t.Fatalf("warning records = %v; want a second episode after overflow drained", got)
	}
}

func TestClassifyRecoveryInventoryCountsOverflowAsExhaustion(t *testing.T) {
	for name, tc := range map[string]struct {
		used, limit, overflow int
		want                  string
	}{
		"empty":                    {0, 10, 0, readservice.RecoveryInventoryHealthy},
		"high water":               {8, 10, 0, readservice.RecoveryInventoryWarning},
		"full":                     {10, 10, 0, readservice.RecoveryInventoryExhausted},
		"overflow beats occupancy": {1, 10, 1, readservice.RecoveryInventoryExhausted},
		"no limit":                 {0, 0, 4, readservice.RecoveryInventoryUnavailable},
	} {
		if got := readservice.ClassifyRecoveryInventory(tc.used, tc.limit, tc.overflow); got != tc.want {
			t.Fatalf("%s: state = %q; want %q", name, got, tc.want)
		}
	}
}

// TestRecoveryOverflowWarningNamesTheTier keeps the record honest about what
// overflow costs. It is NOT the pre-#5370 wedge — cleanup succeeded — so an
// operator must not read it as stopped execution, and must not read it as
// nothing either.
func TestRecoveryOverflowWarningNamesTheTier(t *testing.T) {
	message := recoveryInventoryWarningMessage(&readservice.RecoveryInventoryStatus{
		State: readservice.RecoveryInventoryExhausted, Used: 128, Limit: 128, Overflow: 4,
	})
	for _, want := range []string{"4 snapshot(s)", "mirror refs", "without a bundle"} {
		if !strings.Contains(message, want) {
			t.Fatalf("warning message did not name the overflow tier (%q): %s", want, message)
		}
	}
}
