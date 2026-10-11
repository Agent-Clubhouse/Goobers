package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func seedRecoveryRun(t *testing.T, layout instance.Layout, runID string, finished bool) {
	t.Helper()
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1,
		StartedAt: time.Now().UTC().Add(-time.Hour),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if finished {
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func linuxRecoveryInventoryGate(layout instance.Layout) *recoveryInventoryGate {
	gate := newRecoveryInventoryGate(layout, nil, nil)
	gate.goos = "linux"
	return gate
}

// TestRecoveryInventoryReclaimCandidatesCarryRunnableCommands is the portal's
// "what do I run" contract: an elevated reading names the snapshots an
// operator can act on, oldest first, with the exact commands that act on
// them, instead of a link to a guide.
func TestRecoveryInventoryReclaimCandidatesCarryRunnableCommands(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 4)
	now := time.Now().UTC()
	records := make(map[string]string)
	for i := range 4 {
		record := seedPolicyRecoveryRecord(t, layout, i, now)
		records[record.RunID] = record.Ref
	}
	seedRecoveryRun(t, layout, "capacity-000", true)
	seedRecoveryRun(t, layout, "capacity-001", false)
	seedRecoveryRun(t, layout, "capacity-002", true)
	// capacity-003 has no journal at all: nothing can prove it terminal.

	status := linuxRecoveryInventoryGate(layout).Sample(context.Background())
	if status.State != readservice.RecoveryInventoryExhausted {
		t.Fatalf("state = %q; want exhausted", status.State)
	}
	if status.ReclaimCandidatesTotal != 2 || len(status.ReclaimCandidates) != 2 {
		t.Fatalf("candidates = %d of %d; want only the two terminal runs", len(status.ReclaimCandidates), status.ReclaimCandidatesTotal)
	}
	first := status.ReclaimCandidates[0]
	if first.RunID != "capacity-000" || first.Phase != string(journal.PhaseCompleted) || first.Abandoned {
		t.Fatalf("first candidate = %+v; want the oldest terminal run, not yet abandoned", first)
	}
	root := layout.Root
	wantAbandon := "goobers recovery-abandon --run capacity-000 --ref " + records["capacity-000"] +
		" --confirm-digest sha256:" + strings.Repeat("b", 64) + " " + quoteShellArgIfNeeded(root)
	if first.AbandonCommand != wantAbandon {
		t.Fatalf("abandon command = %q\nwant %q", first.AbandonCommand, wantAbandon)
	}
	if want := "goobers trace --summary capacity-000 " + quoteShellArgIfNeeded(root); first.InspectCommand != want {
		t.Fatalf("inspect command = %q; want %q", first.InspectCommand, want)
	}
	for _, want := range []string{"goobers recovery-restore --record ", "record.json", "--repository . --branch recovered/capacity-000"} {
		if !strings.Contains(first.RestoreCommand, want) {
			t.Fatalf("restore command %q omits %q", first.RestoreCommand, want)
		}
	}
	if want := "goobers status --all " + quoteShellArgIfNeeded(root); status.StatusCommand != want {
		t.Fatalf("status command = %q; want %q", status.StatusCommand, want)
	}

	// Abandoning through the real command marks the snapshot pending reap
	// and stops offering the abandon command for it.
	second := status.ReclaimCandidates[1]
	if err := abandonRecoveryRecord(context.Background(), layout, second.RunID, second.Ref, second.PatchDigest); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	after := linuxRecoveryInventoryGate(layout).Sample(context.Background())
	abandoned := after.ReclaimCandidates[1]
	if !abandoned.Abandoned || abandoned.AbandonCommand != "" {
		t.Fatalf("abandoned candidate = %+v; want marked abandoned with no abandon command", abandoned)
	}
}

func quoteShellArgIfNeeded(arg string) string {
	return operatorCommand("linux", arg)
}

// The read model must stay small however large the inventory grows.
func TestRecoveryInventoryReclaimCandidatesAreBounded(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 12)
	now := time.Now().UTC()
	for i := range 12 {
		record := seedPolicyRecoveryRecord(t, layout, i, now)
		seedRecoveryRun(t, layout, record.RunID, true)
	}
	status := linuxRecoveryInventoryGate(layout).Sample(context.Background())
	if status.ReclaimCandidatesTotal != 12 || len(status.ReclaimCandidates) != readservice.RecoveryReclaimCandidateLimit {
		t.Fatalf("candidates = %d of %d; want %d of 12", len(status.ReclaimCandidates), status.ReclaimCandidatesTotal, readservice.RecoveryReclaimCandidateLimit)
	}
	if status.ReclaimCandidates[0].RunID != "capacity-000" {
		t.Fatalf("first candidate = %q; want the oldest", status.ReclaimCandidates[0].RunID)
	}
}

// A healthy reading carries no commands: nothing needs reclaiming.
func TestRecoveryInventoryHealthyReadingCarriesNoReclaimGuidance(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 10)
	record := seedPolicyRecoveryRecord(t, layout, 0, time.Now().UTC())
	seedRecoveryRun(t, layout, record.RunID, true)
	status := linuxRecoveryInventoryGate(layout).Sample(context.Background())
	if status.State != readservice.RecoveryInventoryHealthy || status.ReclaimCandidates != nil || status.ReclaimHold != nil || status.StatusCommand != "" {
		t.Fatalf("healthy reading = %+v; want no reclaim guidance", status)
	}
}

// An abandonment frees a slot only when a retention pass deletes it, so the
// reading must say which setting is stopping that pass.
func TestRecoveryReclaimHoldNamesTheBlockingSetting(t *testing.T) {
	now := time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
	writeConfig := func(t *testing.T, retention string) instance.Layout {
		t.Helper()
		layout := instance.NewLayout(t.TempDir())
		body := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n" + retention
		if err := os.WriteFile(layout.ConfigFile(), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return layout
	}
	writeGrace := func(t *testing.T, layout instance.Layout, enforceAt time.Time) {
		t.Helper()
		state := retentionGraceState{DetectedAt: enforceAt.Add(-retentionGraceWindow), EnforceAt: enforceAt}
		if err := writeRetentionGraceState(layout, worktreeRetentionStateFile, worktreeRetentionStateSchema, state); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("disabled", func(t *testing.T) {
		hold := recoveryReclaimHold(writeConfig(t, "retention:\n  enabled: false\n"), nil, now)
		if hold == nil || hold.Reason != readservice.RecoveryReclaimHoldDisabled || hold.Setting != "retention.enabled: true" {
			t.Fatalf("hold = %+v; want disabled", hold)
		}
	})
	t.Run("dry-run", func(t *testing.T) {
		hold := recoveryReclaimHold(writeConfig(t, "retention:\n  dryRun: true\n"), nil, now)
		if hold == nil || hold.Reason != readservice.RecoveryReclaimHoldDryRun || hold.Setting != "retention.dryRun: false" {
			t.Fatalf("hold = %+v; want dry-run", hold)
		}
	})
	t.Run("grace not started", func(t *testing.T) {
		layout := writeConfig(t, "")
		hold := recoveryReclaimHold(layout, nil, now)
		if hold == nil || hold.Reason != readservice.RecoveryReclaimHoldGrace || hold.Until != nil || hold.ConfigFile != layout.ConfigFile() {
			t.Fatalf("hold = %+v; want an unstarted grace hold naming instance.yaml", hold)
		}
	})
	t.Run("grace running", func(t *testing.T) {
		layout := writeConfig(t, "")
		writeGrace(t, layout, now.Add(48*time.Hour))
		hold := recoveryReclaimHold(layout, nil, now)
		if hold == nil || hold.Reason != readservice.RecoveryReclaimHoldGrace || hold.Until == nil || !hold.Until.Equal(now.Add(48*time.Hour)) {
			t.Fatalf("hold = %+v; want grace until enforcement", hold)
		}
	})
	t.Run("grace elapsed", func(t *testing.T) {
		layout := writeConfig(t, "")
		writeGrace(t, layout, now.Add(-time.Hour))
		if hold := recoveryReclaimHold(layout, nil, now); hold != nil {
			t.Fatalf("hold = %+v; want none once enforcement began", hold)
		}
	})
	t.Run("immediate", func(t *testing.T) {
		if hold := recoveryReclaimHold(writeConfig(t, "retention:\n  firstEnable: immediate\n"), nil, now); hold != nil {
			t.Fatalf("hold = %+v; want none with firstEnable: immediate", hold)
		}
	})
}

func TestOperatorCommandQuotesForTheHostShell(t *testing.T) {
	if got := operatorCommand("windows", "goobers", "status", "--all", `C:\Goobers\it's here`); got != `goobers status --all 'C:\Goobers\it''s here'` {
		t.Fatalf("windows = %q", got)
	}
	if got := operatorCommand("linux", "goobers", "status", "--all", "/var/lib/goobers dir"); got != `goobers status --all '/var/lib/goobers dir'` {
		t.Fatalf("linux = %q", got)
	}
	if got := operatorCommand("linux", "goobers", "trace", "--summary", "run-1", "/var/lib/goobers"); got != "goobers trace --summary run-1 /var/lib/goobers" {
		t.Fatalf("bare = %q", got)
	}
	if got := operatorCommand("windows", "--record", filepath.Join("a", "record.json")); strings.Contains(got, "''") {
		t.Fatalf("unexpected escaping in %q", got)
	}
}
