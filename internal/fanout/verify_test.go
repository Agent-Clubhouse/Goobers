package fanout

import (
	"encoding/json"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func testMap() apiv1.Map {
	return apiv1.Map{Name: "items", Source: apiv1.FanOutSource{Task: "plan", Slot: "plan"}, Task: "item",
		FailurePolicy: apiv1.BranchContinueOnError, Join: "collate"}
}

func testItems(t *testing.T, identities ...string) []journal.MapItem {
	t.Helper()
	items := make([]journal.MapItem, 0, len(identities))
	for _, identity := range identities {
		item := json.RawMessage(`{"shard":"` + identity + `"}`)
		digest, err := ItemDigest(identity, item)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, journal.MapItem{Identity: identity, Digest: digest, Item: item})
	}
	return items
}

func mapStarted(items []journal.MapItem) journal.Event {
	return journal.Event{Seq: 4, Type: journal.EventMapStarted, Parallel: "items", MapRoster: &journal.MapRoster{
		Source:        journal.FanOutSourceRef{Task: "plan", Slot: "plan", Visit: 2, Attempt: 1, Digest: knownDigest, Integrity: apiv1.IntegrityDerived},
		FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrent: 1, Items: items,
	}}
}

func branchEvent(typ journal.EventType, branch int, item journal.MapItem) journal.Event {
	return journal.Event{Type: typ, Branch: branch, Parallel: "items", BranchName: item.Identity,
		MapItemIdentity: item.Identity, MapItemDigest: item.Digest, Stage: "item"}
}

func assertIntegrity(t *testing.T, err error, kind string) {
	t.Helper()
	var typed *IntegrityError
	if !errors.Is(err, ErrWorkflowIntegrity) || !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed %s", err, CodeWorkflowIntegrity)
	}
	if typed.Kind != kind || typed.ErrorClass() != kind || typed.ErrorCode() != CodeWorkflowIntegrity {
		t.Fatalf("error = %+v, want kind %q", typed, kind)
	}
}

func TestNewMapVisitReverifiesTheRecordedRoster(t *testing.T) {
	items := testItems(t, "a", "b", "c")
	if _, err := NewMapVisit(mapStarted(items), testMap()); err != nil {
		t.Fatalf("valid roster refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*journal.Event, *apiv1.Map)
		kind   string
	}{
		{"no roster", func(e *journal.Event, _ *apiv1.Map) { e.MapRoster = nil }, KindMissing},
		{"duplicate identity", func(e *journal.Event, _ *apiv1.Map) {
			e.MapRoster.Items = []journal.MapItem{items[0], items[0], items[2]}
		}, KindDuplicate},
		{"unsorted", func(e *journal.Event, _ *apiv1.Map) {
			e.MapRoster.Items = []journal.MapItem{items[1], items[0], items[2]}
		}, KindConflicting},
		{"tampered item", func(e *journal.Event, _ *apiv1.Map) {
			tampered := append([]journal.MapItem(nil), items...)
			tampered[1].Item = json.RawMessage(`{"shard":"z"}`)
			e.MapRoster.Items = tampered
		}, KindConflicting},
		{"tampered digest", func(e *journal.Event, _ *apiv1.Map) {
			tampered := append([]journal.MapItem(nil), items...)
			tampered[1].Digest = items[2].Digest
			e.MapRoster.Items = tampered
		}, KindConflicting},
		{"different map", func(e *journal.Event, _ *apiv1.Map) { e.Parallel = "other" }, KindConflicting},
		{"different source", func(e *journal.Event, _ *apiv1.Map) { e.MapRoster.Source.Slot = "other" }, KindConflicting},
		{"different bounds", func(_ *journal.Event, m *apiv1.Map) { m.MaxConcurrent = 4 }, KindConflicting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, def := mapStarted(items), testMap()
			roster := *start.MapRoster
			start.MapRoster = &roster
			tc.mutate(&start, &def)
			_, err := NewMapVisit(start, def)
			assertIntegrity(t, err, tc.kind)
		})
	}
}

// Design §14 case 14 at the verifier level: missing, extra, duplicate and
// digest-conflicting item records fail closed.
func TestMapVisitObserveRefusesCorruptItemRecords(t *testing.T) {
	items := testItems(t, "a", "b")
	stage := func(typ journal.EventType, branch int) journal.Event {
		return journal.Event{Type: typ, Branch: branch, Stage: "item"}
	}
	valid := []journal.Event{
		branchEvent(journal.EventBranchStarted, 1, items[0]),
		stage(journal.EventStageStarted, 1),
		stage(journal.EventStageFinished, 1),
		branchEvent(journal.EventBranchFinished, 1, items[0]),
		branchEvent(journal.EventBranchStarted, 2, items[1]),
		stage(journal.EventStageStarted, 2),
	}
	visit, err := NewMapVisit(mapStarted(items), testMap())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range valid {
		if err := visit.Observe(e); err != nil {
			t.Fatalf("valid record %+v refused: %v", e, err)
		}
	}
	conflicting := branchEvent(journal.EventBranchStarted, 1, items[0])
	conflicting.MapItemDigest = items[1].Digest
	renamed := branchEvent(journal.EventBranchStarted, 1, items[0])
	renamed.BranchName = "b"
	wrongTask := stage(journal.EventStageStarted, 1)
	wrongTask.Stage = "collate"
	// fail_fast settles a queued item as cancelled without starting it.
	cancelledUnstarted := branchEvent(journal.EventBranchFinished, 1, items[0])
	cancelledUnstarted.BranchStatus = journal.BranchCancelled
	if visit, err := NewMapVisit(mapStarted(items), testMap()); err != nil || visit.Observe(cancelledUnstarted) != nil {
		t.Fatalf("an unstarted item cancelled by fail_fast was refused: %v", err)
	}
	for _, tc := range []struct {
		name    string
		records []journal.Event
		kind    string
	}{
		{"finish without start", []journal.Event{branchEvent(journal.EventBranchFinished, 1, items[0])}, KindMissing},
		{"stage after an unstarted cancel", []journal.Event{cancelledUnstarted, stage(journal.EventStageStarted, 1)}, KindMissing},
		{"unstarted cancel twice", []journal.Event{cancelledUnstarted, cancelledUnstarted}, KindDuplicate},
		{"stage before start", []journal.Event{stage(journal.EventStageStarted, 1)}, KindMissing},
		{"branch outside roster", []journal.Event{branchEvent(journal.EventBranchStarted, 3, items[0])}, KindExtra},
		{"stage outside roster", []journal.Event{stage(journal.EventStageFinished, 9)}, KindExtra},
		{"started twice", []journal.Event{valid[0], valid[0]}, KindDuplicate},
		{"finished twice", []journal.Event{valid[0], valid[3], valid[3]}, KindDuplicate},
		{"digest conflicts with roster", []journal.Event{conflicting}, KindConflicting},
		{"identity conflicts with roster", []journal.Event{branchEvent(journal.EventBranchStarted, 1, items[1])}, KindConflicting},
		{"branch name conflicts with roster", []journal.Event{renamed}, KindConflicting},
		{"stage after finish", []journal.Event{valid[0], valid[3], stage(journal.EventStageStarted, 1)}, KindConflicting},
		{"foreign task in item branch", []journal.Event{valid[0], wrongTask}, KindConflicting},
		{"branch record on root", []journal.Event{branchEvent(journal.EventBranchStarted, 0, items[0])}, KindConflicting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visit, err := NewMapVisit(mapStarted(items), testMap())
			if err != nil {
				t.Fatal(err)
			}
			for i, e := range tc.records {
				err = visit.Observe(e)
				if i < len(tc.records)-1 && err != nil {
					t.Fatalf("setup record %d refused: %v", i, err)
				}
			}
			assertIntegrity(t, err, tc.kind)
		})
	}
}

func TestMapVisitJoinInterventionReopensOneItem(t *testing.T) {
	items := testItems(t, "a")
	visit, err := NewMapVisit(mapStarted(items), testMap())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []journal.Event{
		branchEvent(journal.EventBranchStarted, 1, items[0]),
		branchEvent(journal.EventBranchFinished, 1, items[0]),
		{Type: journal.EventRunResumed, Branch: 1, Target: "@join"},
		branchEvent(journal.EventBranchFinished, 1, items[0]),
	} {
		if err := visit.Observe(e); err != nil {
			t.Fatalf("record %+v refused: %v", e, err)
		}
	}
}

func TestMapVisitVerifyFinished(t *testing.T) {
	items := testItems(t, "a", "b")
	outcome := func(branch int, item journal.MapItem) journal.BranchOutcome {
		return journal.BranchOutcome{Branch: branch, Name: item.Identity, Status: journal.BranchSucceeded,
			MapItemIdentity: item.Identity, MapItemDigest: item.Digest}
	}
	finished := func(entries ...journal.BranchOutcome) journal.Event {
		return journal.Event{Type: journal.EventMapFinished, Parallel: "items", Completeness: entries, Target: "collate"}
	}
	visit, err := NewMapVisit(mapStarted(items), testMap())
	if err != nil {
		t.Fatal(err)
	}
	if err := visit.VerifyFinished(finished(outcome(1, items[0]), outcome(2, items[1]))); err != nil {
		t.Fatalf("valid completeness refused: %v", err)
	}
	conflicting := outcome(2, items[1])
	conflicting.MapItemDigest = items[0].Digest
	for _, tc := range []struct {
		name  string
		event journal.Event
		kind  string
	}{
		{"missing item", finished(outcome(1, items[0])), KindMissing},
		{"extra item", finished(outcome(1, items[0]), outcome(2, items[1]), outcome(3, items[1])), KindExtra},
		{"duplicate item", finished(outcome(1, items[0]), outcome(1, items[0])), KindDuplicate},
		{"digest conflict", finished(outcome(1, items[0]), conflicting), KindConflicting},
		{"out of order", finished(outcome(2, items[1]), outcome(1, items[0])), KindConflicting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertIntegrity(t, visit.VerifyFinished(tc.event), tc.kind)
		})
	}
	empty, err := NewMapVisit(mapStarted([]journal.MapItem{}), testMap())
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.VerifyFinished(finished()); err != nil {
		t.Fatalf("an empty roster closes with an empty completeness record: %v", err)
	}
}
