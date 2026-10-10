package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParentRecoverySelectsLatestPhysicalOwner(t *testing.T) {
	for _, mode := range []string{"current", "finished", "replacement", "late-stale", "sibling", "other-stage"} {
		t.Run(mode, func(t *testing.T) {
			_, run, frame := childOriginRuntime(t, &childOriginGoober{})
			if err := frame.recordTaskStartedWithRecovery(1, "", 0, 0, newStageUsageTotals()); err != nil {
				t.Fatal(err)
			}
			original := *frame.childOrigin
			scope := journal.Event{Stage: frame.t.Name, Attempt: 1, Seq: run.Seq()}
			record := func() {
				t.Helper()
				env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, ChildWorkflowOrigin: &original}
				custody := ContainedParentWorkspaceCustody{Version: 1, Origin: &original, Workspace: worktree.StageCustody{OwnerRunID: frame.in.RunID}}
				if err := RecordContainedParentRecovery(run, scope, journal.Digest([]byte("contract")), custody, env, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "late-stale" {
				record()
			}
			if mode == "replacement" || mode == "late-stale" {
				if err := frame.recordTaskStartedWithRecovery(2, journal.AttemptInfra, 0, 0, newStageUsageTotals()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "other-stage" {
				if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "another-stage", Attempt: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "finished" {
				if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: frame.t.Name, Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "late-stale" {
				record()
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			branch := 0
			if mode == "sibling" {
				branch = 1
			}
			_, found, err := selectParentRecovery(reader, events, frame.in.RunID, frame.t.Name, branch)
			if mode == "late-stale" {
				if err == nil || found {
					t.Fatal("stale receipt replaced current owner")
				}
			} else if err != nil || found != (mode == "current") {
				t.Fatal(found, err)
			}
		})
	}
}

func TestParentRecoveryPreservesCumulativeCostAndHumanInstructions(t *testing.T) {
	_, _, frame := childOriginRuntime(t, &childOriginGoober{})
	if err := frame.recordTaskStartedWithRecovery(1, "", 0, 0, newStageUsageTotals()); err != nil {
		t.Fatal(err)
	}
	before := newStageUsageTotals()
	accumulateStageUsage(before, map[string]float64{telemetry.AttrUsageCostUSD: 2})
	if err := frame.recordTaskStartedWithRecovery(2, journal.AttemptInfra, 1, 1, before); err != nil {
		t.Fatal(err)
	}
	frame.containedRecovery = &containedParentRecovery{Custody: ContainedParentWorkspaceCustody{Origin: frame.childOrigin}, Usage: map[string]float64{telemetry.AttrUsageCostUSD: 3}, InstructionAddendum: "old guidance"}
	for _, guidance := range []string{"", "new human guidance"} {
		totals := newStageUsageTotals()
		actual := guidance
		if err := restoreContainedParentUsage(frame, totals, &actual); err != nil {
			t.Fatal(err)
		}
		expected := guidance
		if expected == "" {
			expected = "old guidance"
		}
		if totals.costUSD.RatString() != "5" || actual != expected {
			t.Fatal(totals.costUSD, actual)
		}
	}
}
