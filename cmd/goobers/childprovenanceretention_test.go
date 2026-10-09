package main

import (
	"encoding/json"
	"errors"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stagePruneJournal(t *testing.T, dir string) string {
	t.Helper()
	staged := filepath.Join(filepath.Dir(filepath.Dir(dir)), ".telemetry-pruning", filepath.Base(dir))
	if err := os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, staged); err != nil {
		t.Fatal(err)
	}
	return staged
}

func assertCustodyPreservesJournal(t *testing.T, layout instance.Layout, dir string, now time.Time) {
	t.Helper()
	_, _, err := pruneTelemetryRetention(layout, instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}, nil, now, false)
	if !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("custody was not preserved", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dependent journal removed", err)
	}
	if held, err := journal.PruneReserved(dir); err != nil || held {
		t.Fatal("refusal left prune reservation", held, err)
	}
}

func TestTelemetryPrunePreservesOverflowOwner(t *testing.T) {
	layout := writeRecoveryPolicyInstance(t, 0)
	now := time.Now().UTC()
	dir := createTelemetryRetentionRun(t, layout, "overflow-owner", now.Add(-48*time.Hour))
	inventory := seedLiveRecoveryRecord(t, layout, "overflow-owner", now)
	record, err := recovery.ReadRecord(filepath.Join(inventory, recovery.RecordFileName))
	if err != nil {
		t.Fatal(err)
	}
	record.ArchiveDigest, record.ArchiveBytes, record.ArchiveFormat = "", 0, ""
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	overflow := filepath.Join(recoveryOverflowRoot(layout), filepath.Base(inventory))
	if err := os.MkdirAll(overflow, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overflow, recovery.RecordFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(inventory); err != nil {
		t.Fatal(err)
	}
	assertCustodyPreservesJournal(t, layout, dir, now)
	if err := recovery.DeleteOverflowEntry(recoveryOverflowRoot(layout), record); err != nil {
		t.Fatal(err)
	}
	results, _, err := pruneTelemetryRetention(layout, instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}, nil, now, false)
	if err != nil || len(results) != 1 {
		t.Fatal("retired overflow still holds journal", results, err)
	}
}

func TestTelemetryPrunePreservesChildFamilyUntilTombstone(t *testing.T) {
	for _, mode := range []string{"parent", "child", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			layout := writeRecoveryPolicyInstance(t, 0)
			now := time.Now().UTC()
			if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
				t.Fatal(err)
			}
			queue, err := triggerqueue.Open(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = queue.Close() }()
			identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "example", ParentRunID: "family-parent"}, StageOccurrence: "stage", InvocationKey: "call"}
			child, _, err := queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte("start"), MaxChildren: 1}, now)
			if err != nil {
				t.Fatal(err)
			}
			runID := identity.ParentRunID
			if mode != "parent" {
				runID = child.RunID
			}
			dir := createTelemetryRetentionRun(t, layout, runID, now.Add(-48*time.Hour))
			if mode == "interrupted" {
				dir = stagePruneJournal(t, dir)
			}
			assertCustodyPreservesJournal(t, layout, dir, now)
			if err := queue.SetChildState(t.Context(), identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildCancelled, ResultRef: "result"}, now); err != nil {
				t.Fatal(err)
			}
			if err := queue.AcknowledgeChild(t.Context(), identity, "result", now); err != nil {
				t.Fatal(err)
			}
			if err := queue.MarkChildParentSettled(t.Context(), identity.ChildParent, now); err != nil {
				t.Fatal(err)
			}
			assertCustodyPreservesJournal(t, layout, dir, now)
			if _, err := queue.PruneChildren(t.Context(), now.Add(triggerqueue.ChildRetention), 100); err != nil {
				t.Fatal(err)
			}
			if _, _, err := pruneTelemetryRetention(layout, instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}, nil, now, false); err != nil {
				t.Fatal("settled tombstone blocked history cleanup", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("journal survived released custody", err)
			}
		})
	}
}
