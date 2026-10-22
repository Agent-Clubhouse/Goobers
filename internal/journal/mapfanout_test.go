package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

const (
	testDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigestS = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func testMapRoster() *MapRoster {
	return &MapRoster{
		Source: FanOutSourceRef{Task: "plan", Slot: "plan", Visit: 2, Attempt: 1, Digest: testDigestS,
			Integrity: apiv1.IntegrityDerived, Path: "artifacts/sha256/cc/cc", Size: 99, MediaType: "application/json"},
		FailurePolicy: apiv1.BranchContinueOnError, ItemTimeoutSeconds: 60, MaxConcurrent: 2,
		Items: []MapItem{
			{Identity: "a", Digest: testDigestA, Item: json.RawMessage(`{"shard":"a"}`)},
			{Identity: "b", Digest: testDigestB, Item: json.RawMessage(`{"shard":"b"}`)},
		},
	}
}

func testMapEvents() []Event {
	return []Event{
		{Type: EventMapStarted, Parallel: "items", MapRoster: testMapRoster()},
		{Type: EventBranchStarted, Branch: 1, Parallel: "items", BranchName: "a", MapItemIdentity: "a", MapItemDigest: testDigestA, Stage: "item"},
		{Type: EventBranchFinished, Branch: 1, Parallel: "items", BranchName: "a", MapItemIdentity: "a", MapItemDigest: testDigestA, BranchStatus: BranchSucceeded},
		{Type: EventMapFinished, Parallel: "items", Target: "collate", Completeness: []BranchOutcome{
			{Branch: 1, Name: "a", Status: BranchSucceeded, MapItemIdentity: "a", MapItemDigest: testDigestA},
			{Branch: 2, Name: "b", Status: BranchCancelled, MapItemIdentity: "b", MapItemDigest: testDigestB},
		}},
	}
}

// Every map lifecycle shape the journal emits validates against the shipped
// event schema, and a map.started without its roster is refused at append.
func TestMapEventsMatchSchemaAndRequireRoster(t *testing.T) {
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	run, root := newRun(t)
	for _, ev := range testMapEvents() {
		if err := run.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	empty := Event{Type: EventMapStarted, Parallel: "empty", MapRoster: &MapRoster{
		Source: testMapRoster().Source, FailurePolicy: apiv1.BranchFailFast, MaxConcurrent: 1, Items: []MapItem{},
	}}
	if err := run.Append(empty); err != nil {
		t.Fatalf("Append empty roster: %v", err)
	}
	for _, bad := range []Event{
		{Type: EventMapStarted, Parallel: "items"},
		{Type: EventMapStarted, MapRoster: testMapRoster()},
		{Type: EventMapStarted, Parallel: "items", MapRoster: &MapRoster{FailurePolicy: apiv1.BranchFailFast, MaxConcurrent: 1}},
	} {
		if err := run.Append(bad); err == nil {
			t.Fatalf("map.started without a name or frozen roster was accepted: %+v", bad)
		}
	}
	_ = run.Close()

	f, err := os.Open(filepath.Join(root, testIdentity().RunID, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventBytes)
	seen := map[EventType]bool{}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if err := v.ValidateJSON("journal-event.schema.json", line); err != nil {
			t.Errorf("event fails schema: %v\n%s", err, line)
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatal(err)
		}
		seen[ev.Type] = true
	}
	if !seen[EventMapStarted] || !seen[EventMapFinished] {
		t.Fatalf("map lifecycle events not written: %v", seen)
	}

	missingRoster := []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":"2026-07-13T05:00:00Z","type":"map.started","parallel":"items"}`)
	if err := v.ValidateJSON("journal-event.schema.json", missingRoster); err == nil {
		t.Fatal("schema accepted map.started without mapRoster")
	}
	badIdentity := []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":1,"time":"2026-07-13T05:00:00Z","type":"branch.started","parallel":"items","mapItemIdentity":"-bad"}`)
	if err := v.ValidateJSON("journal-event.schema.json", badIdentity); err == nil {
		t.Fatal("schema accepted a malformed mapItemIdentity")
	}
}

// The map fields join the conformance set: each one distinguishes two
// otherwise identical events, while the roster source's storage metadata stays
// excluded exactly as Ref's is.
// A static parallel's completeness record keeps its historical encoding: the
// map fields only append when present.
// Checkpoint cursors inside a live map visit carry each item's identity and
// digest; map.finished returns the run to a single cursor.
func TestReconstructBranchCursorsMapVisit(t *testing.T) {
	events := testMapEvents()
	open := []Event{events[0], events[1],
		{Type: EventBranchStarted, Branch: 2, Parallel: "items", BranchName: "b", MapItemIdentity: "b", MapItemDigest: testDigestB, Stage: "item"},
		events[2],
	}
	cursors, ok := reconstructBranchCursors(open)
	if !ok || len(cursors) != 2 {
		t.Fatalf("cursors = %+v, %v; want two map item cursors", cursors, ok)
	}
	if cursors[0].ItemIdentity != "a" || cursors[0].ItemDigest != testDigestA || cursors[0].Status != BranchSucceeded || cursors[0].Parallel != "items" {
		t.Errorf("item a cursor = %+v", cursors[0])
	}
	if cursors[1].ItemIdentity != "b" || cursors[1].ItemDigest != testDigestB || cursors[1].MachineState != "item" {
		t.Errorf("item b cursor = %+v", cursors[1])
	}
	if cursors, ok := reconstructBranchCursors(events); ok {
		t.Fatalf("a finished map visit left cursors %+v", cursors)
	}
	// A parallel.finished of the same name does not close a map visit.
	if _, ok := reconstructBranchCursors(append(open, Event{Type: EventParallelFinished, Parallel: "items"})); !ok {
		t.Fatal("parallel.finished closed a map visit")
	}
}

func TestMapLifecycleEndsGateParking(t *testing.T) {
	for _, typ := range []EventType{EventMapStarted, EventMapFinished} {
		if parked := GateParkingAfter(true, Event{Type: typ}); parked {
			t.Fatalf("%s left the run parked at a gate", typ)
		}
	}
}

func TestStageOccurrenceFollowsChildOriginContinuation(t *testing.T) {
	const runID = "run-1"
	first := Event{Seq: 7, Type: EventStageStarted, Branch: 2, Stage: "item", Attempt: 1}
	occurrence := StageOccurrence(runID, first, "")
	if occurrence != StageAttemptID(runID, 2, "item", 7) {
		t.Fatalf("first start occurrence = %s", occurrence)
	}
	retry := Event{Seq: 9, Type: EventStageStarted, Branch: 2, Stage: "item", Attempt: 2, AttemptClass: AttemptPolicy}
	if got := StageOccurrence(runID, retry, occurrence); got != occurrence {
		t.Fatalf("a retry left its occurrence: %s", got)
	}
	revisit := Event{Seq: 12, Type: EventStageStarted, Branch: 2, Stage: "item", Attempt: 1}
	if got := StageOccurrence(runID, revisit, occurrence); got == occurrence {
		t.Fatal("a new visit reused the previous occurrence")
	}
	sibling := Event{Seq: 7, Type: EventStageStarted, Branch: 3, Stage: "item", Attempt: 1}
	if StageOccurrence(runID, sibling, "") == occurrence {
		t.Fatal("two branches share an occurrence")
	}
	bound := StageAttemptID(runID, 2, "item", 3)
	if got := StageOccurrence(runID, Event{Seq: 20, Stage: "item", Attempt: 1, Runner: map[string]any{ChildWorkflowOccurrenceKey: bound}}, ""); got != bound {
		t.Fatalf("bound child occurrence ignored: %s", got)
	}
}
