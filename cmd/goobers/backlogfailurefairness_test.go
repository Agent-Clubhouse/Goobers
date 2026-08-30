package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestDeprioritizeRepeatedFailuresPreservesEventualClaimability(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < backlogFailureDeprioritizeThreshold; i++ {
		runID := "failed-run-" + string(rune('a'+i))
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim("1", runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("claim failed: ok=%t err=%v", ok, err)
		}
		if err := ledger.Release("1", runID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}

	items := []providers.WorkItem{{ID: "1"}, {ID: "2"}}
	got := deprioritizeRepeatedFailures(layout, journalclient.NewFileCrossRun(layout), claimsclient.Listing{Entries: ledger.Snapshot(), History: ledger.HistorySnapshot()}, items, now, backlogQueryEnv{}, "deprioritize-run", "implementation")
	if got[0].ID != "2" || got[1].ID != "1" {
		t.Fatalf("order = %v, want healthy item before repeated failure", []string{got[0].ID, got[1].ID})
	}

	onlyFailed := deprioritizeRepeatedFailures(layout, journalclient.NewFileCrossRun(layout), claimsclient.Listing{Entries: ledger.Snapshot(), History: ledger.HistorySnapshot()}, []providers.WorkItem{{ID: "1"}}, now, backlogQueryEnv{}, "deprioritize-run", "implementation")
	if len(onlyFailed) != 1 || onlyFailed[0].ID != "1" {
		t.Fatalf("deprioritized item = %v, want it retained as claimable", onlyFailed)
	}
}

func TestBacklogQueryClaimDeprioritizesRepeatedFailuresAcrossWorkflows(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		curation bool
	}{
		{name: "implementation", workflow: "implementation"},
		{name: "implementation-critical", workflow: "implementation-critical"},
		{name: "backlog-curation", workflow: "backlog-curation", curation: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(1, "Repeated failure", "goobers", "goobers:ready")
			server.addIssue(2, "Healthy candidate", "goobers", "goobers:ready")
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "claim-run")
			t.Setenv("GOOBERS_WORKFLOW", tt.workflow)
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers")
			if tt.curation {
				t.Setenv("GOOBERS_INPUT_CURATION", "true")
				t.Setenv("GOOBERS_INPUT_MAXITEMS", "1")
				t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
				t.Setenv("GOOBERS_INPUT_STALEAFTERDAYS", "30")
				t.Setenv("GOOBERS_INPUT_STALEAUTOCLOSE", "false")
			} else {
				t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
			}
			seedTerminalFailures(t, root, "1", backlogFailureDeprioritizeThreshold)

			workDir := t.TempDir()
			t.Chdir(workDir)
			code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
			if code != 0 {
				t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if tt.curation {
				data, err := os.ReadFile(filepath.Join(workDir, "claimed-items.json"))
				if err != nil {
					t.Fatal(err)
				}
				var claimed struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(data, &claimed); err != nil {
					t.Fatal(err)
				}
				if claimed.ID != "2" {
					t.Fatalf("claimed items = %+v, want healthy item 2", claimed)
				}
			} else if !strings.Contains(stdout, "claimed 2") {
				t.Fatalf("stdout = %q, want healthy item 2", stdout)
			}
		})
	}
}

func TestTerminalFailureStreakResetsAtSuccessAndWindow(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	seedRun := func(runID string, phase journal.RunPhase, at time.Time) {
		t.Helper()
		now = at
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim("1", runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("claim failed: ok=%t err=%v", ok, err)
		}
		if err := ledger.Release("1", runID); err != nil {
			t.Fatal(err)
		}
	}

	base := now
	seedRun("recent-failure", journal.PhaseFailed, base.Add(-time.Minute))
	seedRun("successful-attempt", journal.PhaseCompleted, base)
	now = base
	if got, degradedAt := terminalFailureStreak(journalclient.NewFileCrossRun(layout), ledger.HistoryForItem("1"), now); got != 0 || degradedAt != "" {
		t.Fatalf("streak after successful attempt = (%d, %q), want (0, \"\")", got, degradedAt)
	}

	seedRun("old-failure", journal.PhaseFailed, base.Add(-backlogFailureWindow-time.Minute))
	now = base
	if got, degradedAt := terminalFailureStreak(journalclient.NewFileCrossRun(layout), ledger.HistoryForItem("1"), now); got != 0 || degradedAt != "" {
		t.Fatalf("streak after out-of-window failure = (%d, %q), want (0, \"\")", got, degradedAt)
	}
}

// TestTerminalFailureStreakDegradesLoudlyOnUnreadablePhase covers the
// terminalFailureStreakDegradedAnnotation path: a claim-history entry whose
// run directory is gone (the pod-without-instance-root shape finding 002
// C3/C4 leaves unaddressed) must not silently truncate the streak to
// whatever count preceded it — terminalFailureStreak has to name the run it
// could not read, and deprioritizeRepeatedFailures has to turn that into a
// journal.EventRunnerAnnotation rather than let the shortfall pass unremarked.
func TestTerminalFailureStreakDegradesLoudlyOnUnreadablePhase(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	// A run whose claim-history entry survives in the ledger but whose run
	// directory is gone — the pod shape: journal.OpenRead / layout.FindRunDir
	// find nothing, exactly as they would with no local instance root at all.
	const missingRunID = "reaped-run"
	if ok, _, err := ledger.Claim("1", missingRunID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim failed: ok=%t err=%v", ok, err)
	}
	if err := ledger.Release("1", missingRunID); err != nil {
		t.Fatal(err)
	}

	streak, degradedAt := terminalFailureStreak(journalclient.NewFileCrossRun(layout), ledger.HistoryForItem("1"), now)
	if streak != 0 {
		t.Fatalf("streak = %d, want 0 (the unreadable entry must not count as a failure)", streak)
	}
	if degradedAt != missingRunID {
		t.Fatalf("degradedAt = %q, want %q naming the unreadable run", degradedAt, missingRunID)
	}

	items := []providers.WorkItem{{ID: "1"}, {ID: "2"}}
	deprioritizeRepeatedFailures(layout, journalclient.NewFileCrossRun(layout), claimsclient.Listing{Entries: ledger.Snapshot(), History: ledger.HistorySnapshot()}, items, now, backlogQueryEnv{stderr: io.Discard}, "watching-run", "implementation")

	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found *journal.Event
	for i := range events {
		if events[i].Type == journal.EventRunnerAnnotation && events[i].Runner["annotation"] == terminalFailureStreakDegradedAnnotation {
			found = &events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no %s annotation in the instance journal: events = %+v", terminalFailureStreakDegradedAnnotation, events)
	}
	if found.RunID != "watching-run" {
		t.Fatalf("annotation RunID = %q, want the backlog-query run that observed the degradation", found.RunID)
	}
}

func seedTerminalFailures(t *testing.T, root, itemID string, count int) {
	t.Helper()
	layout := instance.NewLayout(root)
	now := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		runID := "failed-run-" + string(rune('a'+i))
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("claim failed: ok=%t err=%v", ok, err)
		}
		if err := ledger.Release(itemID, runID); err != nil {
			t.Fatal(err)
		}
	}
}
