package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/recovery"
)

// recoveryInventoryWarnings returns the high-water records an operator would
// find in the instance log, in order.
func recoveryInventoryWarnings(t *testing.T, log *journal.InstanceLog) []string {
	t.Helper()
	events, err := journal.ReadInstanceLog(log.Dir())
	if err != nil {
		t.Fatalf("ReadInstanceLog: %v", err)
	}
	var codes []string
	for _, event := range events {
		if event.Error != nil && strings.HasPrefix(event.Error.Code, "recovery_inventory_") {
			codes = append(codes, event.Error.Code)
		}
	}
	return codes
}

func seedRecoveryRecords(t *testing.T, layout instance.Layout, from, to int) {
	t.Helper()
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	for i := from; i < to; i++ {
		seedPolicyRecoveryRecord(t, layout, i, now)
	}
}

// TestRecoveryInventoryGateClassifiesOccupancy pins the three states #5343
// requires an operator be able to see, against the documented 80% high-water
// threshold rather than an absolute slot count.
func TestRecoveryInventoryGateClassifiesOccupancy(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	gate := newRecoveryInventoryGate(layout, nil, nil)

	seedRecoveryRecords(t, layout, 0, 7)
	if status := gate.Sample(context.Background()); status.State != readservice.RecoveryInventoryHealthy || status.Used != 7 || status.Limit != 10 {
		t.Fatalf("7 of 10 = %+v; want healthy 7/10", status)
	}
	seedRecoveryRecords(t, layout, 7, 8)
	status := gate.Sample(context.Background())
	if status.State != readservice.RecoveryInventoryWarning || status.Used != 8 {
		t.Fatalf("8 of 10 = %+v; want warning at the 80%% high-water mark", status)
	}
	if status.EarliestRetainUntil == nil {
		t.Fatal("warning omitted the earliest retention deadline, which is what says when a slot next frees")
	}
	if status.InventoryRoot != filepath.Join(layout.Root, "recovery") {
		t.Fatalf("InventoryRoot = %q; want the inventory this limit is enforced against", status.InventoryRoot)
	}
	seedRecoveryRecords(t, layout, 8, 10)
	if status := gate.Sample(context.Background()); status.State != readservice.RecoveryInventoryExhausted || status.Used != 10 {
		t.Fatalf("10 of 10 = %+v; want exhausted", status)
	}
	// Stats serves the read model the same reading, not a re-scan.
	if got := gate.Stats(); got == nil || got.State != readservice.RecoveryInventoryExhausted {
		t.Fatalf("Stats() = %+v; want the stored exhausted sample", got)
	}
}

// TestRecoveryInventoryGateResolvesLimitFromInstanceConfigAtPointOfUse is
// #4911's 2026-09-17 mandatory-review correction stated as a regression: the
// reported limit must come from instance.yaml at the moment of the reading,
// not from the *instance.Config the daemon was started with, so an operator
// who changes retention.recovery.maxSnapshots sees it reflected without a
// restart and the visibility surface cannot report stale success-shaped
// capacity.
func TestRecoveryInventoryGateResolvesLimitFromInstanceConfigAtPointOfUse(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	// The carried configuration is deliberately WRONG, and deliberately the
	// generous direction: if it were consulted, 8 entries would read as a
	// healthy 8/128 rather than a warning.
	carried := &instance.Config{Retention: instance.RetentionConfig{Recovery: &instance.RecoverySnapshotConfig{MaxSnapshots: 128}}}
	gate := newRecoveryInventoryGate(layout, carried, nil)
	seedRecoveryRecords(t, layout, 0, 8)

	status := gate.Sample(context.Background())
	if status.Limit != 10 || status.State != readservice.RecoveryInventoryWarning {
		t.Fatalf("carried config won: %+v; want limit 10 and a warning", status)
	}

	// The operator raises the cap. No restart, no new gate: the same live
	// object must resolve the new value on its next reading.
	body := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nretention:\n  recovery:\n    maxSnapshots: " + strconv.Itoa(100) + "\n"
	if err := os.WriteFile(layout.ConfigFile(), []byte(body), 0o600); err != nil {
		t.Fatalf("raise maxSnapshots: %v", err)
	}
	raised := gate.Sample(context.Background())
	if raised.Limit != 100 || raised.State != readservice.RecoveryInventoryHealthy {
		t.Fatalf("after raising the cap: %+v; want limit 100 and healthy", raised)
	}
	if raised.PolicySource != recoveryPolicyFromInstanceConfig {
		t.Fatalf("PolicySource = %q; want %q", raised.PolicySource, recoveryPolicyFromInstanceConfig)
	}

	// And the other direction: lowering it below the current occupancy must
	// report exhaustion rather than the cap the daemon booted with.
	lowered := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nretention:\n  recovery:\n    maxSnapshots: 8\n"
	if err := os.WriteFile(layout.ConfigFile(), []byte(lowered), 0o600); err != nil {
		t.Fatalf("lower maxSnapshots: %v", err)
	}
	if status := gate.Sample(context.Background()); status.Limit != 8 || status.State != readservice.RecoveryInventoryExhausted {
		t.Fatalf("after lowering the cap: %+v; want limit 8 and exhausted", status)
	}
}

// TestRecoveryInventoryCountsIncompleteReservations pins that a reservation
// holding no interpretable record still occupies a slot, and that one of them
// does not take the whole reading down (#5177): the strict read fails outright
// on such a directory, so a capacity report built on it would go blank exactly
// when capacity is the problem.
func TestRecoveryInventoryCountsIncompleteReservations(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	seedRecoveryRecords(t, layout, 0, 6)
	debris := filepath.Join(layout.Root, "recovery", strings.Repeat("d", 64))
	if err := os.MkdirAll(debris, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(debris, ".publish.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	status := newRecoveryInventoryGate(layout, nil, nil).Sample(context.Background())
	if status.State == readservice.RecoveryInventoryUnavailable {
		t.Fatalf("one incomplete reservation made the whole reading unavailable: %+v", status)
	}
	if status.Used != 7 || status.Unreadable != 1 {
		t.Fatalf("occupancy = %d used / %d unreadable; want 7 and 1 (incomplete reservations hold slots)", status.Used, status.Unreadable)
	}
	// 7 of 10 is below the high-water mark: the count is what matters here,
	// not the classification, so state must follow the same arithmetic.
	if status.State != readservice.RecoveryInventoryHealthy {
		t.Fatalf("state = %q; want %q for 7/10", status.State, readservice.RecoveryInventoryHealthy)
	}
}

// TestRecoveryInventoryReadsAboveTheConfiguredCap covers the "130 of 128"
// shape from #5354, where the READ refuses rather than the reservation. An
// observer that passed the cap down would report nothing at the one occupancy
// that most needs reporting.
func TestRecoveryInventoryReadsAboveTheConfiguredCap(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 4)
	seedRecoveryRecords(t, layout, 0, 6)

	status := newRecoveryInventoryGate(layout, nil, nil).Sample(context.Background())
	if status.State != readservice.RecoveryInventoryExhausted || status.Used != 6 || status.Limit != 4 {
		t.Fatalf("over-cap inventory = %+v; want exhausted 6/4", status)
	}
}

// TestRecoveryInventoryUnreadableConfigurationIsNotHealthy keeps an
// unmeasurable inventory explicit: it must never be indistinguishable from an
// empty one.
func TestRecoveryInventoryUnreadableConfigurationIsNotHealthy(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.WriteFile(filepath.Join(layout.Root, "recovery"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	status := newRecoveryInventoryGate(layout, nil, nil).Sample(context.Background())
	if status.State != readservice.RecoveryInventoryUnavailable || status.Error == "" {
		t.Fatalf("unreadable inventory = %+v; want an explained unavailable reading", status)
	}
}

// TestRecoveryInventoryHighWaterWarningIsDeduplicated is #4911 AC5's
// deduplication requirement: once on the way into warning, once on the way
// into exhausted, nothing while the condition is unchanged, and re-armed by
// dropping back below the threshold.
func TestRecoveryInventoryHighWaterWarningIsDeduplicated(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	log := openTestInstanceLog(t)
	at := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	report := func(used int) {
		status := &readservice.RecoveryInventoryStatus{
			State: readservice.ClassifyRecoveryInventory(used, 10, 0), Used: used, Limit: 10,
			HighWaterPercent: readservice.RecoveryInventoryHighWaterPercent,
			InventoryRoot:    filepath.Join(layout.Root, "recovery"),
		}
		reportRecoveryInventoryHealth(layout, log, status, at)
	}

	report(4)
	report(8)
	report(8)
	report(9)
	if got := recoveryInventoryWarnings(t, log); len(got) != 1 || got[0] != "recovery_inventory_high_water" {
		t.Fatalf("warning records = %v; want exactly one high-water record", got)
	}
	report(10)
	report(10)
	if got := recoveryInventoryWarnings(t, log); len(got) != 2 || got[1] != "recovery_inventory_exhausted" {
		t.Fatalf("warning records = %v; want the exhausted record reported once", got)
	}
	// Falling back below the mark re-arms, so a second episode is reported as
	// a second episode rather than swallowed by the first.
	report(4)
	report(8)
	if got := recoveryInventoryWarnings(t, log); len(got) != 3 || got[2] != "recovery_inventory_high_water" {
		t.Fatalf("warning records = %v; want a re-armed high-water record", got)
	}
}

// TestRecoveryInventoryWarningSurvivesDaemonRestart is why the dedupe state is
// a file rather than a field. A wedged instance restarts often, and that is
// exactly when repeating the same record on every start is least useful.
func TestRecoveryInventoryWarningSurvivesDaemonRestart(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	at := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	status := &readservice.RecoveryInventoryStatus{
		State: readservice.RecoveryInventoryExhausted, Used: 10, Limit: 10,
		HighWaterPercent: readservice.RecoveryInventoryHighWaterPercent,
	}

	first := openTestInstanceLog(t)
	reportRecoveryInventoryHealth(layout, first, status, at)
	if got := recoveryInventoryWarnings(t, first); len(got) != 1 {
		t.Fatalf("first start recorded %v; want one record", got)
	}
	// A restart is a new process with a new log handle and no memory of the
	// previous one. The durable state is all that stands between an unchanged
	// condition and a duplicate record.
	second := openTestInstanceLog(t)
	reportRecoveryInventoryHealth(layout, second, status, at.Add(time.Hour))
	if got := recoveryInventoryWarnings(t, second); len(got) != 0 {
		t.Fatalf("restart with an unchanged condition recorded %v; want nothing new", got)
	}
	if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), recoveryInventoryWarningStateFile)); err != nil {
		t.Fatalf("dedupe state is not durable: %v", err)
	}
}

// TestRecoveryInventoryUnavailableDoesNotRearmTheWarning keeps a transient
// read failure from manufacturing a duplicate: an unmeasured sample is not
// evidence the condition cleared.
func TestRecoveryInventoryUnavailableDoesNotRearmTheWarning(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	log := openTestInstanceLog(t)
	at := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	exhausted := &readservice.RecoveryInventoryStatus{State: readservice.RecoveryInventoryExhausted, Used: 10, Limit: 10}

	reportRecoveryInventoryHealth(layout, log, exhausted, at)
	reportRecoveryInventoryHealth(layout, log, &readservice.RecoveryInventoryStatus{State: readservice.RecoveryInventoryUnavailable, Error: "locked"}, at)
	reportRecoveryInventoryHealth(layout, log, exhausted, at)

	if got := recoveryInventoryWarnings(t, log); len(got) != 1 {
		t.Fatalf("warning records = %v; want the exhausted record once", got)
	}
}

// TestRecoveryInventoryWarningNamesTheConsequence keeps the record readable by
// the operator who will otherwise meet this condition as an unrelated stage
// failing at `create worktree`.
func TestRecoveryInventoryWarningNamesTheConsequence(t *testing.T) {
	deadline := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	message := recoveryInventoryWarningMessage(&readservice.RecoveryInventoryStatus{
		State: readservice.RecoveryInventoryExhausted, Used: 128, Limit: 128, Unreadable: 3,
		InventoryRoot: filepath.Join("instances", "fixture", "recovery"), EarliestRetainUntil: &deadline,
	})
	for _, want := range []string{"128 of 128", "worktree cleanup", "unrelated runs", "incomplete reservation", deadline.Format(time.RFC3339)} {
		if !strings.Contains(message, want) {
			t.Fatalf("warning %q omits %q", message, want)
		}
	}
}

// TestServiceHealthRecordCarriesRecoveryInventory is #4911 AC5's daemon-health
// half: the six-hour record carries occupancy, so an operator reading the
// history back can see whether the instance was under capacity pressure at the
// time rather than only that something failed.
func TestServiceHealthRecordCarriesRecoveryInventory(t *testing.T) {
	deadline := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	sample := &readservice.RecoveryInventoryStatus{
		State: readservice.RecoveryInventoryWarning, Used: 104, Limit: 128, Unreadable: 2,
		HighWaterPercent: readservice.RecoveryInventoryHighWaterPercent,
		InventoryRoot:    "recovery", PolicySource: recoveryPolicyFromInstanceConfig,
		EarliestRetainUntil: &deadline,
	}
	payload := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, nil, func() *readservice.RecoveryInventoryStatus { return sample }, time.Unix(1_700_000_000, 0)))

	inventory, ok := payload["recoveryInventory"].(map[string]any)
	if !ok {
		t.Fatalf("payload carries no recovery inventory: %+v", payload)
	}
	for key, want := range map[string]any{
		"state": readservice.RecoveryInventoryWarning, "used": 104, "limit": 128,
		"unreadable": 2, "highWaterPercent": readservice.RecoveryInventoryHighWaterPercent,
	} {
		if inventory[key] != want {
			t.Errorf("recoveryInventory[%q] = %v, want %v", key, inventory[key], want)
		}
	}
	// A reader with no sample declines to answer rather than reporting an
	// empty inventory it never measured.
	bare := serviceHealthPayload(observeServiceHealth(t.TempDir(), nil, nil, nil, time.Unix(1_700_000_000, 0)))
	if _, present := bare["recoveryInventory"]; present {
		t.Errorf("unmeasured record claims an inventory reading: %+v", bare)
	}
}

// TestRecoveryInventoryTickerStopsWithContext keeps the daemon's shutdown join
// honest: the ticker goroutine must close its done channel on cancellation.
func TestRecoveryInventoryTickerStopsWithContext(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	setup := &schedulerSetup{InstanceLog: openTestInstanceLog(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := startRecoveryInventoryTicker(ctx, layout, setup, newRecoveryInventoryGate(layout, nil, nil), time.Hour)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery inventory ticker did not stop on cancellation")
	}
}

// A zero interval is a disabled ticker, not a busy loop.
func TestRecoveryInventoryTickerDisabledByZeroInterval(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	setup := &schedulerSetup{InstanceLog: openTestInstanceLog(t)}
	select {
	case <-startRecoveryInventoryTicker(context.Background(), layout, setup, newRecoveryInventoryGate(layout, nil, nil), 0):
	case <-time.After(10 * time.Second):
		t.Fatal("zero interval did not disable the ticker")
	}
}

// The sampled cadence exists so a polled read route never scans the inventory
// under its instance-wide lock on the request path.
func TestRecoveryInventorySampleIntervalIsBounded(t *testing.T) {
	if recoveryInventorySampleInterval <= 0 || recoveryInventorySampleInterval > 5*time.Minute {
		t.Fatalf("recoveryInventorySampleInterval = %v; want a bounded sub-five-minute cadence", recoveryInventorySampleInterval)
	}
	if recovery.MaxInventoryEntries < instance.DefaultRecoverySnapshotMaxCount {
		t.Fatal("the structural ceiling must be able to observe a default-capped inventory in full")
	}
}
