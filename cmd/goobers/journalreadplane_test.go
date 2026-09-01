package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
)

// journalreadplane_test.go covers the daemon side of #3880: the containment
// the filesystem cannot express. Every test here asks the same question in a
// different shape — can a pod learn something about a run that is not its own
// and not in its gaggle?

const crossRunTestGaggle = "acme-web"

func crossRunTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	// The daemon always has a scheduler directory; the claim ledger the
	// unpushed-work route reads lives in it.
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return layout
}

// seedCrossRunRun writes a run journal with a run.yaml beside it, which is
// what the daemon's gaggle containment check keys on.
func seedCrossRunRun(t *testing.T, layout instance.Layout, gaggle, runID string, phase journal.RunPhase) {
	t.Helper()
	runsDir := layout.ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatalf("append run.finished for %s: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %s: %v", runID, err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "run.yaml"),
		[]byte("runId: "+runID+"\ngaggle: "+gaggle+"\n"), 0o644); err != nil {
		t.Fatalf("write run.yaml for %s: %v", runID, err)
	}
}

// TestDaemonRunPhaseRefusesRunsOutsideTheAskingRunsGaggle is the containment
// that makes the phase route safe to expose at all: without it, a pod could
// enumerate the terminal phase of every run on the instance by naming its own
// gaggle and someone else's run id.
func TestDaemonRunPhaseRefusesRunsOutsideTheAskingRunsGaggle(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "asking-run", journal.PhaseCompleted)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "sibling-run", journal.PhaseFailed)
	seedCrossRunRun(t, layout, "other-gaggle", "foreign-run", journal.PhaseFailed)

	service := newDaemonRunJournalService(layout, nil)
	ctx := context.Background()

	response, err := service.RunPhase(ctx, journalclient.RunPhaseRequest{
		RunID: "asking-run", TargetRunID: "sibling-run", Gaggle: crossRunTestGaggle,
	})
	if err != nil {
		t.Fatalf("same-gaggle phase: %v", err)
	}
	if response.Phase != string(journal.PhaseFailed) {
		t.Fatalf("phase = %q, want failed", response.Phase)
	}

	// A run in another gaggle is refused, and refused the SAME way a
	// nonexistent run is — the route must not be a run-id oracle.
	foreign, foreignErr := service.RunPhase(ctx, journalclient.RunPhaseRequest{
		RunID: "asking-run", TargetRunID: "foreign-run", Gaggle: crossRunTestGaggle,
	})
	if foreignErr == nil {
		t.Fatalf("a foreign gaggle's run answered %+v", foreign)
	}
	_, missingErr := service.RunPhase(ctx, journalclient.RunPhaseRequest{
		RunID: "asking-run", TargetRunID: "never-existed", Gaggle: crossRunTestGaggle,
	})
	if missingErr == nil {
		t.Fatal("a nonexistent run answered a phase")
	}
	if foreignErr.Error() != missingErr.Error() {
		t.Fatalf("foreign-gaggle refusal (%v) is distinguishable from nonexistent-run refusal (%v)",
			foreignErr, missingErr)
	}

	// An asking run that is not in the gaggle it named is refused outright.
	if _, err := service.RunPhase(ctx, journalclient.RunPhaseRequest{
		RunID: "asking-run", TargetRunID: "sibling-run", Gaggle: "other-gaggle",
	}); err == nil {
		t.Fatal("an asking run outside the named gaggle was served")
	}
}

// TestDaemonCrossRunRoutesRefuseUnsafeGaggleSegments is the path-traversal
// guard: a gaggle name is a directory segment, so it must be a plain one.
func TestDaemonCrossRunRoutesRefuseUnsafeGaggleSegments(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "asking-run", journal.PhaseCompleted)
	service := newDaemonRunJournalService(layout, nil)
	ctx := context.Background()

	for _, gaggle := range []string{"../other", "a/b", "..", "", "."} {
		if _, err := service.RunPhase(ctx, journalclient.RunPhaseRequest{
			RunID: "asking-run", TargetRunID: "asking-run", Gaggle: gaggle,
		}); err == nil {
			t.Errorf("gaggle %q was accepted for a phase read", gaggle)
		}
		if _, err := service.ConflictTouches(ctx, journalclient.ConflictTouchRequest{
			RunID: "asking-run", Gaggle: gaggle, Since: time.Now().Add(-time.Hour),
		}); err == nil {
			t.Errorf("gaggle %q was accepted for a conflict read", gaggle)
		}
		if _, err := service.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{
			RunID: "asking-run", Gaggle: gaggle, Since: time.Now().Add(-time.Hour),
		}); err == nil {
			t.Errorf("gaggle %q was accepted for an unpushed-work read", gaggle)
		}
	}

	// So must an unsafe run id.
	for _, runID := range []string{"../escape", "a/b", ""} {
		if _, err := service.RunPhase(ctx, journalclient.RunPhaseRequest{
			RunID: "asking-run", TargetRunID: runID, Gaggle: crossRunTestGaggle,
		}); err == nil {
			t.Errorf("target run id %q was accepted", runID)
		}
	}
}

// TestDaemonUnpushedWorkDerivesItemsFromTheLedger is the security property
// the unpushed-work route exists to hold: the asking run's items come from the
// daemon's ledger, so a pod cannot read work stranded on an item it does not
// hold — no matter what it sends.
func TestDaemonUnpushedWorkDerivesItemsFromTheLedger(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "asking-run", journal.PhaseRunning)
	seedStrandedDiffRun(t, layout, crossRunTestGaggle, "prior-run", "42", "diff for item 42")
	seedStrandedDiffRun(t, layout, crossRunTestGaggle, "someone-elses-run", "99", "diff for item 99")

	service := newDaemonRunJournalService(layout, nil)
	ctx := context.Background()
	since := time.Now().UTC().Add(-24 * time.Hour)

	// With no claim held, the asking run gets nothing — even though it asked
	// for item 99 by name.
	work, err := service.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{
		RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since, ItemIDs: []string{"99"},
	})
	if err != nil {
		t.Fatalf("unpushed work with no claim: %v", err)
	}
	if work.Work != nil {
		t.Fatalf("a run holding no claim was served %+v", work.Work)
	}

	// Holding item 42 gets item 42's stranded diff, and STILL not item 99's.
	claimItemForRun(t, layout, "42", "asking-run")
	work, err = service.UnpushedWork(ctx, journalclient.UnpushedWorkRequest{
		RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: since, ItemIDs: []string{"99"},
	})
	if err != nil {
		t.Fatalf("unpushed work holding item 42: %v", err)
	}
	if work.Work == nil {
		t.Fatal("the run holds item 42 but its stranded diff was not offered")
	}
	if work.Work.RunID != "prior-run" {
		t.Fatalf("work.runId = %q, want prior-run — the ledger, not the request, decides",
			work.Work.RunID)
	}
	if !strings.Contains(work.Work.Diff, "item 42") {
		t.Fatalf("diff = %q, want item 42's", work.Work.Diff)
	}
}

// TestDaemonConflictTouchesStaysInsideTheGaggle proves the conflict history a
// pod receives is scoped to its own gaggle's runs.
func TestDaemonConflictTouchesStaysInsideTheGaggle(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "asking-run", journal.PhaseRunning)
	seedConflictRun(t, layout, crossRunTestGaggle, "conflicted-run", "internal/mine.go")
	seedConflictRun(t, layout, "other-gaggle", "foreign-conflicted-run", "internal/theirs.go")

	service := newDaemonRunJournalService(layout, nil)
	response, err := service.ConflictTouches(context.Background(), journalclient.ConflictTouchRequest{
		RunID: "asking-run", Gaggle: crossRunTestGaggle, Since: time.Now().UTC().Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("conflict touches: %v", err)
	}
	if len(response.Touches) != 1 || response.Touches[0].RunID != "conflicted-run" {
		t.Fatalf("touches = %+v, want only this gaggle's conflicted run", response.Touches)
	}
	for _, touch := range response.Touches {
		for _, file := range touch.Files {
			if strings.Contains(file, "theirs") {
				t.Fatalf("another gaggle's conflicting file %q crossed the boundary", file)
			}
		}
	}
}

// TestDaemonRunJournalServiceSatisfiesThePlaneInterface keeps the wiring
// honest: the daemon implementation is what the route calls.
func TestDaemonRunJournalServiceSatisfiesThePlaneInterface(t *testing.T) {
	var _ httpapi.RunJournalService = newDaemonRunJournalService(crossRunTestLayout(t), nil)
}

// --- seeding helpers --------------------------------------------------------

func claimItemForRun(t *testing.T, layout instance.Layout, itemID, runID string) {
	t.Helper()
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if ok, _, err := ledger.Claim(itemID, runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim %s for %s: ok=%t err=%v", itemID, runID, ok, err)
	}
}

func seedStrandedDiffRun(t *testing.T, layout instance.Layout, gaggle, runID, itemID, diff string) {
	t.Helper()
	runsDir := layout.ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	patchRef, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.patch", []byte(diff))
	if err != nil {
		t.Fatalf("record diff for %s: %v", runID, err)
	}
	meta, err := json.Marshal(map[string]any{
		"schema": "goobers.dev/unpushed-diff/v1", "runId": runID, "workflow": "implementation",
		"stage": "implement", "attempt": 1, "itemIds": []string{itemID},
		"branch": "goobers/implementation/" + runID, "baseRef": "main", "diffBytes": len(diff),
		"diff": map[string]any{"path": patchRef.Path, "digest": patchRef.Digest, "size": patchRef.Size},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.json", meta); err != nil {
		t.Fatalf("record sidecar for %s: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %s: %v", runID, err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "run.yaml"), []byte("runId: "+runID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedConflictRun(t *testing.T, layout instance.Layout, gaggle, runID, file string) {
	t.Helper()
	runsDir := layout.ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	payload, err := json.Marshal(map[string]any{
		"code": journalclient.ConflictArtifactCode, "conflictingFiles": []string{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordStageArtifact("sync-base", 1, "", runID+journalclient.ConflictArtifactSuffix, payload); err != nil {
		t.Fatalf("record conflict artifact for %s: %v", runID, err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run %s: %v", runID, err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "run.yaml"), []byte("runId: "+runID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
