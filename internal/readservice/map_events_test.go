package readservice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestProjectEventCarriesMapItemLineage pins the Portal's map-item lineage:
// map.started projects the frozen roster's identities and digests in branch
// order without the item values, and an item's branch events and the
// map.finished completeness keep the item identity and digest.
func TestProjectEventCarriesMapItemLineage(t *testing.T) {
	const digestA = "sha256:" + "aa00000000000000000000000000000000000000000000000000000000000000"
	const digestB = "sha256:" + "bb00000000000000000000000000000000000000000000000000000000000000"
	started := projectEvent(journal.EventRecord{Event: journal.Event{
		Schema: journal.EventSchema, Seq: 4, Type: journal.EventMapStarted, Parallel: "run-shards",
		MapRoster: &journal.MapRoster{
			Source:        journal.FanOutSourceRef{Task: "plan", Slot: "shards", Visit: 2, Attempt: 1, Digest: "sha256:source", Integrity: apiv1.IntegrityDerived, Path: "artifacts/private"},
			FailurePolicy: apiv1.BranchContinueOnError, ItemTimeoutSeconds: 600, MaxConcurrent: 2,
			Items: []journal.MapItem{
				{Identity: "unit-a", Digest: digestA, Item: json.RawMessage(`{"secretish":"item-value-marker"}`)},
				{Identity: "unit-b", Digest: digestB, Item: json.RawMessage(`{}`)},
			},
		},
	}}, artifactIndex{})
	want := &MapRosterSummary{SourceTask: "plan", SourceSlot: "shards", SourceDigest: "sha256:source", Integrity: apiv1.IntegrityDerived,
		FailurePolicy: apiv1.BranchContinueOnError, ItemTimeoutSeconds: 600, MaxConcurrent: 2,
		Items: []MapItemSummary{{Branch: 1, Identity: "unit-a", Digest: digestA}, {Branch: 2, Identity: "unit-b", Digest: digestB}}}
	if !reflect.DeepEqual(started.MapRoster, want) {
		t.Fatalf("map.started roster = %+v, want %+v", started.MapRoster, want)
	}
	raw, err := json.Marshal(started)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"item-value-marker", "artifacts/private"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("projected map.started leaked %q: %s", private, raw)
		}
	}

	branch := projectEvent(journal.EventRecord{Event: journal.Event{
		Schema: journal.EventSchema, Seq: 5, Type: journal.EventBranchStarted, Branch: 1, Parallel: "run-shards",
		BranchName: "unit-a", MapItemIdentity: "unit-a", MapItemDigest: digestA,
	}}, artifactIndex{})
	if branch.MapItemIdentity != "unit-a" || branch.MapItemDigest != digestA {
		t.Fatalf("branch.started lost map item lineage: %+v", branch)
	}

	finished := projectEvent(journal.EventRecord{Event: journal.Event{
		Schema: journal.EventSchema, Seq: 9, Type: journal.EventMapFinished, Parallel: "run-shards",
		Completeness: []journal.BranchOutcome{{Branch: 1, Name: "unit-a", Status: journal.BranchSucceeded, MapItemIdentity: "unit-a", MapItemDigest: digestA}},
	}}, artifactIndex{})
	if len(finished.Completeness) != 1 || finished.Completeness[0].MapItemDigest != digestA || finished.MapRoster != nil {
		t.Fatalf("map.finished projection = %+v", finished)
	}
}
