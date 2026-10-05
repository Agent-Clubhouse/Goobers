package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestParentContributionPrePruneProtectsBeforeFirstChild(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 0)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: "retained-parent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	guard, closeGuard, err := openTriggerPruneGuard(layout, false, time.Now())
	if err != nil || guard == nil {
		t.Fatal("missing queue removed contribution guard", err)
	}
	defer closeGuard()
	candidate := retention.Result{RunID: "retained-parent", RunDir: run.Dir()}
	if err := guard(candidate); err != nil {
		t.Fatal("ordinary journal blocked", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": runner.ContainedParentWorkspaceKind}}); err != nil {
		t.Fatal(err)
	}
	if err := guard(candidate); err == nil {
		t.Fatal("unresolved contribution journal pruned")
	}
}

func TestParentContributionPreventsCompletedFamilySettlement(t *testing.T) {
	f := actualChildLaunchFixture(t)
	family := &childFamilyLifecycle{layout: f.launcher.layout, queue: f.service.queue, runners: f.launcher.runners}
	parent := triggerqueue.ChildParent{Gaggle: f.submission.Envelope.Gaggle, ParentRunID: f.submission.Envelope.ParentRunID}
	dir, err := f.launcher.layout.FindRunDir(parent.ParentRunID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": runner.ContainedParentWorkspaceKind}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := family.settle(t.Context(), parent); err == nil {
		t.Fatal("unarchived contribution allowed family settlement")
	}
	parents, err := f.service.queue.UnsettledChildParents(t.Context(), triggerqueue.ChildParent{}, 100)
	if err != nil || len(parents) != 1 {
		t.Fatal(parents, err)
	}
}
