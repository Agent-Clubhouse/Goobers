package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/journal"
)

// This file is the FAN-3 (#7384) corpus for map durability: roster freezing
// from the producer's journal-bound attempt, strict resume reconstruction,
// and the per-occurrence result store. The TestConformanceMap* functions are
// design plan-driven-dynamic-fan-out.md §14 cases 12, 13 and 14; every one is
// driven by journal fixtures, because map execution itself lands with FAN-4.

// mapProducerJournal creates a run journal whose root-branch producer "plan"
// published roster as its "plan" slot through a named artifact-set index,
// exactly as an artifact publication records it.
func mapProducerJournal(t *testing.T, roster []byte) (string, []journal.Event) {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID: "map-freeze", Workflow: "fanout", WorkflowVersion: 1, WorkflowDigest: journal.Digest([]byte("fanout")),
		Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	visit := run.Seq()
	payloadRef, err := run.RecordArtifact("plan", roster)
	if err != nil {
		t.Fatal(err)
	}
	payload := apiv1.ArtifactPointer{Path: payloadRef.Path, Digest: payloadRef.Digest, Size: payloadRef.Size,
		MediaType: "application/json", Integrity: apiv1.IntegrityDerived}
	index, err := json.Marshal(artifactset.Index{
		SchemaVersion: artifactset.NamedSchemaVersion,
		Entries:       []artifactset.Entry{{Name: "plan", Slot: 1, Artifact: payload}},
		Bindings:      []artifactset.SlotBinding{{Stage: "plan", Visit: visit, Attempt: 1, Slot: "plan", Artifact: payload}},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexRef, err := run.RecordArtifact("artifact-set.json", index)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{
			{Path: indexRef.Path, Digest: indexRef.Digest, Size: indexRef.Size, MediaType: "application/json", Integrity: apiv1.IntegrityDerived},
			{Path: payload.Path, Digest: payload.Digest, Size: payload.Size, MediaType: payload.MediaType, Integrity: payload.Integrity},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenRead(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return run.Dir(), events
}

func testMapDef(t *testing.T) apiv1.Map {
	def, ok := mapMachine(t).Map("items")
	if !ok {
		t.Fatal("map items missing")
	}
	return def
}

func assertNoMapStarted(t *testing.T, events []journal.Event) {
	t.Helper()
	for _, e := range events {
		if e.Type == journal.EventMapStarted {
			t.Fatalf("map.started recorded although the roster did not freeze: %+v", e)
		}
	}
}

func TestFreezeMapRosterFromJournalBoundAttempt(t *testing.T) {
	roster := `{"schemaVersion":1,"items":[{"identity":"unit-b","item":{"n":2}},{"identity":"unit-a","item":{"n":1}}]}`
	dir, events := mapProducerJournal(t, []byte(roster))
	def := testMapDef(t)
	started, err := freezeMapRoster(context.Background(), dir, events, def)
	if err != nil {
		t.Fatal(err)
	}
	if started.Type != journal.EventMapStarted || started.Parallel != "items" || started.MapRoster == nil {
		t.Fatalf("event = %+v, want map.started for items", started)
	}
	got := started.MapRoster
	if got.Source.Task != "plan" || got.Source.Slot != "plan" || got.Source.Attempt != 1 || got.Source.Visit == 0 ||
		got.Source.Digest != journal.Digest([]byte(roster)) || got.Source.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("source = %+v, want the journal-bound plan attempt", got.Source)
	}
	if got.FailurePolicy != def.FailurePolicy || got.MaxConcurrent != 1 || len(got.Items) != 2 ||
		got.Items[0].Identity != "unit-a" || got.Items[1].Identity != "unit-b" {
		t.Fatalf("roster = %+v, want normalized items and effective bounds", got)
	}
	// The event is appendable as-is and then reconstructs as a pending visit.
	if _, err := fanout.NewMapVisit(journal.Event{Type: started.Type, Parallel: started.Parallel, MapRoster: got}, def); err != nil {
		t.Fatalf("frozen roster does not re-verify: %v", err)
	}
}

func TestFreezeMapRosterRefusesUnboundOrFailedProducers(t *testing.T) {
	def := testMapDef(t)
	t.Run("no producer attempt", func(t *testing.T) {
		_, err := freezeMapRoster(context.Background(), t.TempDir(), nil, def)
		assertMapSourceInvalid(t, err, fanout.ReasonProducer)
	})
	t.Run("failed producer", func(t *testing.T) {
		dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[]}`))
		events[len(events)-1].Status = string(apiv1.ResultFailure)
		_, err := freezeMapRoster(context.Background(), dir, events, def)
		assertMapSourceInvalid(t, err, fanout.ReasonProducer)
	})
	t.Run("binding from another attempt", func(t *testing.T) {
		dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[]}`))
		for i := range events {
			if events[i].Type == journal.EventStageStarted {
				events[i].Seq-- // still precedes the finish; the index binds the original visit
			}
		}
		_, err := freezeMapRoster(context.Background(), dir, events, def)
		assertMapSourceInvalid(t, err, fanout.ReasonProducer)
	})
	t.Run("wrong media type", func(t *testing.T) {
		dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[]}`))
		last := &events[len(events)-1]
		last.Artifacts = append([]journal.Ref(nil), last.Artifacts...)
		last.Artifacts[1].MediaType = "text/plain"
		_, err := freezeMapRoster(context.Background(), dir, events, def)
		// The positional pointer no longer matches the index entry, so the
		// publication itself is refused before any payload byte is read.
		assertMapSourceInvalid(t, err, fanout.ReasonProducer)
	})
	t.Run("empty roster freezes", func(t *testing.T) {
		dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[]}`))
		started, err := freezeMapRoster(context.Background(), dir, events, def)
		if err != nil || started.MapRoster.Items == nil || len(started.MapRoster.Items) != 0 {
			t.Fatalf("empty roster = %+v, %v", started.MapRoster, err)
		}
	})
}

// Design §14 case 12: duplicate identities fail before map.started.
func TestConformanceMapDuplicateIdentitiesFailBeforeMapStarted(t *testing.T) {
	dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[{"identity":"dup","item":{"n":1}},{"identity":"dup","item":{"n":2}}]}`))
	_, err := freezeMapRoster(context.Background(), dir, events, testMapDef(t))
	assertMapSourceInvalid(t, err, fanout.ReasonDuplicateIdentity)
	assertNoMapStarted(t, events)
}

// Design §14 case 13: a malformed or mismatched supplied digest fails before
// map.started, an omitted digest is computed, and verification excludes the
// digest field itself.
func TestConformanceMapDigestVerificationBeforeMapStarted(t *testing.T) {
	item := json.RawMessage(`{"n":1}`)
	canonical, err := fanout.ItemDigest("unit-a", item)
	if err != nil {
		t.Fatal(err)
	}
	def := testMapDef(t)
	for _, tc := range []struct {
		name, digest, reason string
	}{
		{"malformed", `,"digest":"sha256:NOT-HEX"`, fanout.ReasonEnvelope},
		{"mismatched", `,"digest":"sha256:` + strings.Repeat("0", 64) + `"`, fanout.ReasonDigestMismatch},
		{"over the whole entry", `,"digest":"` + wholeEntryDigest(t, "unit-a", item) + `"`, fanout.ReasonDigestMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[{"identity":"unit-a","item":{"n":1}`+tc.digest+`}]}`))
			_, err := freezeMapRoster(context.Background(), dir, events, def)
			assertMapSourceInvalid(t, err, tc.reason)
			assertNoMapStarted(t, events)
		})
	}
	for name, supplied := range map[string]string{"omitted": "", "matching": `,"digest":"` + canonical + `"`} {
		t.Run(name, func(t *testing.T) {
			dir, events := mapProducerJournal(t, []byte(`{"schemaVersion":1,"items":[{"identity":"unit-a","item":{"n":1}`+supplied+`}]}`))
			started, err := freezeMapRoster(context.Background(), dir, events, def)
			if err != nil {
				t.Fatal(err)
			}
			if got := started.MapRoster.Items[0].Digest; got != canonical {
				t.Fatalf("frozen digest = %s, want canonical %s", got, canonical)
			}
		})
	}
}

func wholeEntryDigest(t *testing.T, identity string, item json.RawMessage) string {
	t.Helper()
	// Treat the digest field as if it were inside the hashed value. Because
	// it is not, this can never be the canonical digest.
	data, err := json.Marshal(map[string]any{"identity": identity, "item": item, "digest": ""})
	if err != nil {
		t.Fatal(err)
	}
	return journal.Digest(data)
}

func assertMapSourceInvalid(t *testing.T, err error, reason string) {
	t.Helper()
	var typed *fanout.SourceError
	if !errors.Is(err, fanout.ErrSourceInvalid) || !errors.As(err, &typed) || typed.Reason != reason {
		t.Fatalf("error = %v, want %s/%s", err, fanout.CodeSourceInvalid, reason)
	}
}

// mapResumeFixture is a journal fixture of an interrupted map visit over
// three items: item a settled, item b mid-stage, item c not yet started.
func mapResumeFixture(t *testing.T) ([]journal.Event, []journal.MapItem) {
	t.Helper()
	items := make([]journal.MapItem, 0, 3)
	for _, identity := range []string{"a", "b", "c"} {
		item := json.RawMessage(`{"shard":"` + identity + `"}`)
		digest, err := fanout.ItemDigest(identity, item)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, journal.MapItem{Identity: identity, Digest: digest, Item: item})
	}
	branch := func(typ journal.EventType, id int, status journal.BranchStatus) journal.Event {
		item := items[id-1]
		return journal.Event{Type: typ, Branch: id, Parallel: "items", BranchName: item.Identity,
			MapItemIdentity: item.Identity, MapItemDigest: item.Digest, Stage: "item", BranchStatus: status}
	}
	stage := func(typ journal.EventType, id int, outputs map[string]any) journal.Event {
		e := journal.Event{Type: typ, Branch: id, Stage: "item", Attempt: 1, Outputs: outputs}
		if typ == journal.EventStageFinished {
			e.Status = string(apiv1.ResultSuccess)
		}
		return e
	}
	events := []journal.Event{
		{Type: journal.EventRunStarted},
		{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: string(apiv1.ResultSuccess)},
		{Type: journal.EventMapStarted, Parallel: "items", MapRoster: &journal.MapRoster{
			Source:        journal.FanOutSourceRef{Task: "plan", Slot: "plan", Visit: 2, Attempt: 1, Digest: items[0].Digest, Integrity: apiv1.IntegrityDerived},
			FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrent: 1, Items: items,
		}},
		branch(journal.EventBranchStarted, 1, ""),
		stage(journal.EventStageStarted, 1, nil),
		stage(journal.EventStageFinished, 1, map[string]any{"result": "a-out"}),
		branch(journal.EventBranchFinished, 1, journal.BranchSucceeded),
		branch(journal.EventBranchStarted, 2, ""),
		stage(journal.EventStageStarted, 2, nil),
	}
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	return events, items
}

func TestPendingFanOutRebuildsMapVisitFromRoster(t *testing.T) {
	events, items := mapResumeFixture(t)
	machine := mapMachine(t)
	par, start, err := pendingParallel(events, machine)
	if err != nil {
		t.Fatal(err)
	}
	if par == nil || start != 3 || par.spec.Name != "items" || len(par.branches) != 3 {
		t.Fatalf("pending = %+v at %d, want the 3-item map visit at index 3", par, start)
	}
	cursors := par.cursors()
	for i, item := range items {
		if cursors[i].Branch != i+1 || cursors[i].Name != item.Identity || cursors[i].ItemIdentity != item.Identity || cursors[i].ItemDigest != item.Digest {
			t.Fatalf("cursor %d = %+v, want roster item %q", i, cursors[i], item.Identity)
		}
	}
	if cursors[0].Status != journal.BranchSucceeded || cursors[1].MachineState != "item" || cursors[1].Status != "" || par.branches[2].started {
		t.Fatalf("cursors = %+v, want a settled, b running, c unstarted", cursors)
	}
	if par.spec.Join != "collate" || par.spec.MaxConcurrentBranches != 1 || par.branches[0].start != "item" {
		t.Fatalf("expanded spec = %+v, want the map's join, bound and task", par.spec)
	}
	// Static reconstruction is untouched: a finished map leaves nothing pending.
	finished := append(append([]journal.Event(nil), events[:8]...), journal.Event{
		Seq: 9, Type: journal.EventMapFinished, Parallel: "items", Target: "collate",
		Completeness: par.completeness(),
	})
	finished[8].Completeness[0].Status = journal.BranchSucceeded
	if par, _, err := pendingParallel(finished, machine); err != nil || par != nil {
		t.Fatalf("finished visit = %+v, %v; want nothing pending", par, err)
	}
}

// Design §14 case 14: missing, extra, duplicate and digest-conflicting resume
// events fail closed with a typed workflow_integrity refusal, and resume never
// falls back to the source task.
func TestConformanceMapCorruptResumeEventsFailClosed(t *testing.T) {
	machine := mapMachine(t)
	for _, tc := range []struct {
		name   string
		mutate func([]journal.Event, []journal.MapItem) []journal.Event
		kind   string
	}{
		{"missing branch.started", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			return append(e[:4:4], e[5:]...)
		}, fanout.KindMissing},
		{"missing map.started", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			out := append(e[:3:3], e[4:]...)
			return append(out, journal.Event{Type: journal.EventMapFinished, Parallel: "items"})
		}, fanout.KindMissing},
		{"missing completeness entry", func(e []journal.Event, items []journal.MapItem) []journal.Event {
			return append(e, journal.Event{Type: journal.EventMapFinished, Parallel: "items", Target: "collate", Completeness: []journal.BranchOutcome{
				{Branch: 1, Name: "a", Status: journal.BranchSucceeded, MapItemIdentity: "a", MapItemDigest: items[0].Digest},
			}})
		}, fanout.KindMissing},
		{"extra branch", func(e []journal.Event, items []journal.MapItem) []journal.Event {
			return append(e, journal.Event{Type: journal.EventBranchStarted, Branch: 4, Parallel: "items", BranchName: "d",
				MapItemIdentity: "d", MapItemDigest: items[0].Digest, Stage: "item"})
		}, fanout.KindExtra},
		{"extra stage record", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			return append(e, journal.Event{Type: journal.EventStageFinished, Branch: 7, Stage: "item"})
		}, fanout.KindExtra},
		{"duplicate branch.started", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			return append(e, e[8])
		}, fanout.KindDuplicate},
		{"duplicate branch.finished", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			return append(e, e[7])
		}, fanout.KindDuplicate},
		{"duplicate map.started", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			return append(e, e[3])
		}, fanout.KindDuplicate},
		{"digest conflicts with roster", func(e []journal.Event, items []journal.MapItem) []journal.Event {
			e[8].MapItemDigest = items[2].Digest
			return e
		}, fanout.KindConflicting},
		{"identity conflicts with roster", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			e[7].MapItemIdentity = "c"
			return e
		}, fanout.KindConflicting},
		{"roster item tampered", func(e []journal.Event, items []journal.MapItem) []journal.Event {
			roster := *e[3].MapRoster
			roster.Items = append([]journal.MapItem(nil), items...)
			roster.Items[1].Item = json.RawMessage(`{"shard":"z"}`)
			e[3].MapRoster = &roster
			return e
		}, fanout.KindConflicting},
		{"map not in pinned definition", func(e []journal.Event, _ []journal.MapItem) []journal.Event {
			e[3].Parallel = "renamed"
			return e
		}, fanout.KindConflicting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, items := mapResumeFixture(t)
			events = tc.mutate(events, items)
			par, start, err := pendingParallel(events, machine)
			var typed *fanout.IntegrityError
			if !errors.Is(err, fanout.ErrWorkflowIntegrity) || !errors.As(err, &typed) || typed.Kind != tc.kind {
				t.Fatalf("pendingParallel = %v, want %s/%s", err, fanout.CodeWorkflowIntegrity, tc.kind)
			}
			if par != nil || start != -1 {
				t.Fatalf("a refused reconstruction returned state %+v at %d", par, start)
			}
		})
	}
}

// Two occurrences of one task name keep distinct outputs in the
// per-occurrence store, where the stage-name store collapses them (§6.3).
func TestFanOutResultsKeepRepeatedTaskOccurrencesDistinct(t *testing.T) {
	events, _ := mapResumeFixture(t)
	events = append(events, journal.Event{Seq: uint64(len(events) + 1), Type: journal.EventStageFinished, Branch: 2, Stage: "item",
		Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"result": "b-out"}, Integrity: apiv1.IntegrityMaintainer})
	machine := mapMachine(t)
	const runID = "map-results"

	// The stage-name store would keep only the last item, so it never holds
	// a mapped result at all.
	if byName := reconstructStageOutputs(events, machine); byName["item"].outputs != nil {
		t.Fatalf("stage-name store kept a mapped result: %v", byName["item"].outputs)
	}

	store := reconstructFanOutResults(runID, events, machine)
	visit := events[3].Seq
	a, okA := store.branchResult("items", visit, 1, "item")
	b, okB := store.branchResult("items", visit, 2, "item")
	if !okA || !okB || a.Outputs["result"] != "a-out" || b.Outputs["result"] != "b-out" {
		t.Fatalf("per-occurrence results = %+v / %+v, want a-out and b-out", a, b)
	}
	if b.Integrity != apiv1.IntegrityMaintainer {
		t.Fatalf("item b integrity = %s, want the producing content's grade", b.Integrity)
	}
	occurrenceA := journal.StageAttemptID(runID, 1, "item", events[5].Seq)
	if exact, ok := store.results[fanOutResultKey{FanOut: "items", Branch: 1, Stage: "item", Occurrence: occurrenceA}]; !ok || exact.Outputs["result"] != "a-out" {
		t.Fatalf("exact occurrence lookup = %+v, %v", exact, ok)
	}
	if _, ok := store.branchResult("items", visit+100, 1, "item"); ok {
		t.Fatal("a result leaked into another visit")
	}

	// A second visit of the same map re-runs the same task on the same branch
	// id; its occurrence is new, and both visits stay addressable.
	next := uint64(len(events))
	second := append(append([]journal.Event(nil), events...),
		journal.Event{Seq: next + 1, Type: journal.EventMapFinished, Parallel: "items", Target: "collate", Completeness: []journal.BranchOutcome{}},
	)
	second = append(second, journal.Event{Seq: next + 2, Type: journal.EventMapStarted, Parallel: "items", MapRoster: events[3].MapRoster},
		journal.Event{Seq: next + 3, Type: journal.EventStageStarted, Branch: 1, Stage: "item", Attempt: 1},
		journal.Event{Seq: next + 4, Type: journal.EventStageFinished, Branch: 1, Stage: "item", Attempt: 1, Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"result": "a-again"}},
	)
	store = reconstructFanOutResults(runID, second, machine)
	first, _ := store.branchResult("items", visit, 1, "item")
	again, _ := store.branchResult("items", next+2, 1, "item")
	if first.Outputs["result"] != "a-out" || again.Outputs["result"] != "a-again" {
		t.Fatalf("visits collapsed: first %+v, second %+v", first, again)
	}

	// A tolerated failure is forgotten, as stageOutputs.RecordCompleted does.
	tolerant := newFanOutResults()
	key := fanOutResultKey{FanOut: "items", Branch: 1, Stage: "item", Occurrence: occurrenceA}
	tolerant.record(key, visit, apiv1.Task{Name: "item"}, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"x": 1}}, nil)
	tolerant.record(key, visit, apiv1.Task{Name: "item", ContinueOnError: true}, apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, nil)
	if _, ok := tolerant.results[key]; ok {
		t.Fatal("a tolerated failure kept its occurrence's earlier result")
	}
}
