//go:build integration

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationTerminalFinalizationRenewsOverflowLikeABundle is #5403's
// first acceptance: a run whose capture went to the overflow tier carries the
// same retainUntil after terminal finalization as a run whose capture was
// bundled — its finish time plus the retain window, not its capture time plus
// the window.
func TestIntegrationTerminalFinalizationRenewsOverflowLikeABundle(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 1)
	f.writeConfig()
	f.seed([]reclaimEntry{
		{runID: "renew-bundled", ageHours: 4, terminal: true, files: map[string]string{"bundled.txt": "bundled work\n"}},
	})
	captured := f.overflowSeed("renew-overflow", 4, "overflowed work\n")

	for _, runID := range []string{"renew-bundled", "renew-overflow"} {
		if err := finalizeTerminalRun(f.layout, nil, f.manager, runID); err != nil {
			t.Fatalf("finalize %s: %v", runID, err)
		}
	}
	bundled := f.retainedRecord("renew-bundled")
	if want := f.terminalDeadline("renew-bundled"); !bundled.RetainUntil.Equal(want) {
		t.Fatalf("bundle-backed deadline = %s, want %s", bundled.RetainUntil, want)
	}
	overflow := f.overflowRecordFor("renew-overflow")
	want := f.terminalDeadline("renew-overflow")
	if !overflow.RetainUntil.Equal(want) {
		t.Fatalf("overflow deadline = %s, want %s (capture-time deadline was %s)", overflow.RetainUntil, want, captured.RetainUntil)
	}
	if !f.journaledOverflowDeadline("renew-overflow", want) {
		t.Fatal("the overflow renewal was not journaled with recoveryOverflow=true and its new deadline")
	}
}

// TestIntegrationTerminalOverflowRenewalSkipsAnUnresolvablePin pins the
// best-effort half of the design: an overflow record whose pin is gone has
// nothing restorable to extend, and that must never defer terminal
// finalization (#5354) — the record keeps its capture-time deadline and the
// skip is journaled so the unrenewed deadline is explicable.
func TestIntegrationTerminalOverflowRenewalSkipsAnUnresolvablePin(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 1)
	f.writeConfig()
	f.seed(nil)
	captured := f.overflowSeed("renew-unpinned", 4, "pin removed\n")
	recoveryCLIGit(t, f.mirror, "update-ref", "-d", captured.Ref)
	if err := finalizeTerminalRun(f.layout, nil, f.manager, "renew-unpinned"); err != nil {
		t.Fatalf("an unresolvable overflow pin deferred terminal finalization: %v", err)
	}
	if got := f.overflowRecordFor("renew-unpinned").RetainUntil; !got.Equal(captured.RetainUntil) {
		t.Fatalf("an overflow record whose pin is gone was renewed: got %s, want %s", got, captured.RetainUntil)
	}
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	skipped := false
	for _, event := range events {
		if event.Error != nil && event.Error.Code == recovery.OverflowRenewalSkippedCode && strings.Contains(event.Error.Message, captured.Ref) {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("the skipped overflow renewal was not journaled")
	}
}

// TestIntegrationOverflowPromotionPreservesTheRenewedDeadline is #5403's
// second acceptance: promotion moves an entry between tiers and must carry a
// renewed deadline into the bundle, never reverting to the capture-time one.
func TestIntegrationOverflowPromotionPreservesTheRenewedDeadline(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 1)
	f.writeConfig()
	f.seed([]reclaimEntry{
		{runID: "promote-hold", ageHours: 9, terminal: true, files: map[string]string{"hold.txt": "held\n"}},
	})
	f.overflowSeed("promote-renewed", 4, "renewed then promoted\n")
	if err := finalizeTerminalRun(f.layout, nil, f.manager, "promote-renewed"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	renewed := f.overflowRecordFor("promote-renewed").RetainUntil

	f.retireSeeded("promote-hold")
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if remaining := f.overflowRunIDs(); remaining["promote-renewed"] {
		t.Fatalf("the renewed overflow entry was not promoted: %v", remaining)
	}
	if got := f.retainedRecord("promote-renewed").RetainUntil; !got.Equal(renewed) {
		t.Fatalf("promotion changed the renewed deadline: got %s, want %s", got, renewed)
	}
}

// TestIntegrationInterruptedPromotionCarriesALaterRenewal covers the window
// between a promotion's bundle publication and its removal of the overflow
// record. If a renewal moves the overflow deadline in that window, the retry
// no longer matches the bundle's published deadline; it must carry the move
// onto the bundle and finish, not fail every pass as a conflict.
func TestIntegrationInterruptedPromotionCarriesALaterRenewal(t *testing.T) {
	testdep.Require(t, "git")
	f := newReclaimFixture(t, 2)
	f.writeConfig()
	f.seed(nil)
	captured := f.overflowSeed("promote-interrupted", 4, "interrupted promotion\n")
	// The first half of a promotion: the bundle at the capture-time deadline.
	if _, _, err := recovery.PublishToInventoryWithEviction(t.Context(), f.mirror, f.root, []string{f.mirror}, captured, f.cap, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	later := captured.RetainUntil.Add(12 * time.Hour)
	if _, err := recovery.RenewOverflowRetention(t.Context(), f.mirror, f.overflowRecordPath("promote-interrupted"), later); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runExpirySweep(false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if remaining := f.overflowRunIDs(); remaining["promote-interrupted"] {
		t.Fatalf("the interrupted promotion did not complete: %v", remaining)
	}
	if got := f.retainedRecord("promote-interrupted").RetainUntil; !got.Equal(later) {
		t.Fatalf("the bundle did not carry the later renewal: got %s, want %s", got, later)
	}
}

// writeConfig persists the fixture's config: terminal renewal resolves an
// overflow record's managed repository from the instance's own config file.
func (f *reclaimFixture) writeConfig() {
	f.t.Helper()
	cfg := *f.cfg
	cfg.Repos = append([]instance.RepoRef(nil), f.cfg.Repos...)
	// A persisted config must validate; the in-memory fixture never needed a
	// credential reference, and renewal never reads one.
	cfg.Repos[0].Token = instance.TokenRef{Env: "RECOVERY_TEST_TOKEN"}
	if err := instance.WriteConfig(f.layout.ConfigFile(), &cfg); err != nil {
		f.t.Fatal(err)
	}
}

// terminalDeadline is the deadline terminal renewal must produce for runID:
// its durable finish time plus the effective retain window.
func (f *reclaimFixture) terminalDeadline(runID string) time.Time {
	f.t.Helper()
	reader, err := journal.OpenReadOnly(filepath.Join(f.layout.RunsDir(), runID))
	if err != nil {
		f.t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		f.t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		f.t.Fatal(err)
	}
	finishedAt, err := recoveryWindowTime(events, identity.StartedAt)
	if err != nil {
		f.t.Fatal(err)
	}
	policy, _ := resolveRecoveryPolicy(f.layout, f.cfg)
	window, err := policy.RetainWindowEffective()
	if err != nil {
		f.t.Fatal(err)
	}
	return finishedAt.Add(window)
}

func (f *reclaimFixture) overflowEntry(runID string) recovery.InventoryEntry {
	f.t.Helper()
	entries, _, err := readConfiguredRecoveryOverflow(f.t.Context(), f.layout)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Record.RunID == runID {
			return entry
		}
	}
	f.t.Fatalf("no overflow record for run %s", runID)
	return recovery.InventoryEntry{}
}

func (f *reclaimFixture) overflowRecordFor(runID string) recovery.Record {
	f.t.Helper()
	return f.overflowEntry(runID).Record
}

func (f *reclaimFixture) overflowRecordPath(runID string) string {
	f.t.Helper()
	return f.overflowEntry(runID).RecordPath
}

func (f *reclaimFixture) journaledOverflowDeadline(runID string, deadline time.Time) bool {
	f.t.Helper()
	events, err := journal.ReadInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		f.t.Fatal(err)
	}
	want := deadline.UTC().Format(time.RFC3339Nano)
	for _, event := range events {
		if event.RunID == runID && event.Runner["recoveryOverflow"] == true && event.Runner["recoveryRetainUntil"] == want {
			return true
		}
	}
	return false
}
