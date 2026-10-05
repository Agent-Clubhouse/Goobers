package journal

import (
	"path/filepath"
	"strings"
	"testing"
)

func eventIdentity() RunIdentity {
	runID := strings.Repeat("a", 32)
	return RunIdentity{RunID: runID, Gaggle: "g", Workflow: "consumer", WorkflowVersion: 3, WorkflowDigest: Digest([]byte("workflow")), GooberDigest: Digest(nil), ConfigGeneration: Digest([]byte("config")), Trigger: Trigger{Kind: TriggerSignal, Ref: "event:group"}, Event: &EventLineage{Gaggle: "g", GroupID: "group", Consumer: "subscription", Revision: "v1", AcceptanceID: "trigger-" + runID, EnvelopeDigest: Digest([]byte("start")), ManifestDigest: Digest([]byte("manifest"))}}
}

func TestEventJournalIdentityRefusesMixedOrIncompleteCustody(t *testing.T) {
	cases := map[string]func(*RunIdentity){
		"foreign":      func(id *RunIdentity) { id.Event.Gaggle = "other" },
		"receipt":      func(id *RunIdentity) { id.Event.AcceptanceID = "trigger-other" },
		"child":        func(id *RunIdentity) { id.Child = &ChildLineage{} },
		"continuation": func(id *RunIdentity) { id.ContinuedFromRunID = "other" },
		"trigger":      func(id *RunIdentity) { id.Trigger.Kind = TriggerManual },
		"digest":       func(id *RunIdentity) { id.Event.ManifestDigest = "sha256:wrong" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			id := eventIdentity()
			edit(&id)
			if _, err := Create(t.TempDir(), id, nil); err == nil {
				t.Fatal("invalid provenance published")
			}
		})
	}
}

func TestEventContinuationPreservesSourceButOwnsSeparateExecution(t *testing.T) {
	root := t.TempDir()
	id := eventIdentity()
	source, err := Create(root, id, map[string][]byte{"event-payload": []byte("original")})
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenReadOnly(filepath.Join(root, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := CreateContinuation(root, ContinuationRequest{RunID: strings.Repeat("b", 32), SourceRunID: id.RunID, ExpectedTerminalSeq: events[len(events)-1].Seq, Operator: "human", Target: "stage", Inputs: map[string][]byte{"event-payload": []byte("original")}})
	if err != nil {
		t.Fatal(err)
	}
	if err = epoch.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := OpenReadOnly(filepath.Join(root, strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	continued, err := next.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if continued.Event != nil || continued.ContinuedFromRunID != id.RunID || len(continued.Inputs) != 1 {
		t.Fatal("continuation copied event execution identity or lost inputs")
	}
	original, err := rd.Identity()
	if err != nil || original.Event == nil {
		t.Fatal("source lineage changed", err)
	}
}
