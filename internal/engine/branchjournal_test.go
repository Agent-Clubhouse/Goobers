package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func TestBranchJournalLiveAndProjectionPreserveAttribution(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	rec := &runJournal{proj: JournalProjection{Identity: journal.RunIdentity{RunID: "branch-journal", Gaggle: "web", Workflow: "branches", WorkflowVersion: 1, WorkflowDigest: "sha256:test"}, Graph: json.RawMessage(`{}`), Definition: json.RawMessage(`{}`)}}
	rec.appendAt(at, journal.Event{Type: journal.EventRunStarted, Status: string(journal.PhaseRunning)})
	blob := []byte(`{"event":"branch transcript"}`)
	source := &fakeSpanSource{blobs: map[string][]byte{journal.Digest(blob): blob}}
	// Identical stage/attempt/name on separate branches must have distinct keys
	// and verdict refs, including when one branch has not recorded its gate yet.
	for _, branch := range []int{1, 2} {
		rec.appendAt(at, journal.Event{Type: journal.EventStageStarted, Branch: branch, Stage: "shared", Attempt: 1})
		rec.artifactAt(at, JournalArtifactOp{Branch: branch, Name: "verdict", Data: []byte(fmt.Sprintf(`{"branch":%d}`, branch))})
		rec.artifactAt(at, JournalArtifactOp{Branch: branch, Stage: "shared", Attempt: 1, Name: "context", Data: []byte(`{}`)})
		rec.spanAt(at, JournalSpanOp{Branch: branch, Stage: "shared", Name: "transcript", Ref: journal.Ref{Digest: journal.Digest(blob)}})
	}
	for _, branch := range []int{2, 1} {
		rec.appendAt(at, journal.Event{Type: journal.EventGateEvaluated, Branch: branch, Gate: "review", Name: "verdict"})
	}
	rec.appendAt(at, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	rec.assignEmitKeys()
	keys := map[string]bool{}
	for _, op := range rec.proj.Ops {
		if keys[op.EmitKey] {
			t.Fatalf("branch retry-key collision: %s", op.EmitKey)
		}
		keys[op.EmitKey] = true
	}
	if rec.proj.Ops[1].EmitKey != "branch-journal|1|shared|1|0" || rec.proj.Ops[5].EmitKey != "branch-journal|2|shared|1|0" {
		t.Fatalf("branch key allocation: %+v", rec.proj.Ops)
	}
	liveDir := t.TempDir()
	writer, err := livejournal.NewWriter(func(string) (string, bool) { return liveDir, true }, livejournal.WithSpanSource(source))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	req := livejournal.EmitRequest{RunID: rec.proj.Identity.RunID, Gaggle: "web", Open: &livejournal.OpenHeader{Identity: rec.proj.Identity, Graph: rec.proj.Graph, Definition: rec.proj.Definition}}
	for _, op := range rec.proj.Ops {
		req.Ops = append(req.Ops, liveOpFrom(op))
	}
	// Rehydrate after the artifacts, before either verdict references them.
	req.Ops = req.Ops[:9]
	if _, err := writer.Emit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	writer.CloseIdle(0)
	for _, op := range rec.proj.Ops[9:] {
		if _, err := writer.Emit(context.Background(), livejournal.EmitRequest{RunID: req.RunID, Gaggle: req.Gaggle, Ops: []livejournal.Op{liveOpFrom(op)}}); err != nil {
			t.Fatal(err)
		}
	}
	req.Ops = nil
	for _, op := range rec.proj.Ops[1:] {
		req.Ops = append(req.Ops, liveOpFrom(op))
	}
	if response, err := writer.Emit(context.Background(), req); err != nil || response.Applied != 0 || response.Deduplicated != len(req.Ops) {
		t.Fatalf("retry: %+v %v", response, err)
	}
	projected, err := ProjectRun(t.TempDir(), rec.proj, WithSpanSource(source))
	if err != nil {
		t.Fatal(err)
	}
	live := branchJournalView(t, filepath.Join(liveDir, req.RunID))
	repaired := branchJournalView(t, projected)
	if !reflect.DeepEqual(live, repaired) {
		t.Fatalf("live/projection branch drift:\n%v\n%v", live, repaired)
	}
	for _, branch := range []int{1, 2} {
		for _, kind := range []journal.EventType{journal.EventStageStarted, journal.EventArtifactRecorded, journal.EventSpanRecorded, journal.EventGateEvaluated} {
			if live[fmt.Sprintf("%d/%s", branch, kind)] == "" {
				t.Fatalf("missing branch %d %s: %v", branch, kind, live)
			}
		}
		if got := live[fmt.Sprintf("%d/%s", branch, journal.EventGateEvaluated)]; got != journal.Digest([]byte(fmt.Sprintf(`{"branch":%d}`, branch))) {
			t.Fatalf("branch %d borrowed sibling verdict: %s", branch, got)
		}
	}
}

func branchJournalView(t *testing.T, dir string) map[string]string {
	t.Helper()
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, event := range events {
		value := "present"
		if event.Ref != nil {
			value = event.Ref.Digest
		}
		out[fmt.Sprintf("%d/%s", event.Branch, event.Type)] = value
	}
	return out
}

func TestBranchJournalLegacyPayloadOmitsBranch(t *testing.T) {
	for _, op := range []JournalOp{{Kind: opArtifact, Artifact: &JournalArtifactOp{Name: "a"}}, {Kind: opSpan, Span: &JournalSpanOp{Name: "s"}}, {Kind: opAppend, Event: &journal.Event{Type: journal.EventStageStarted, Stage: "task", Attempt: 1}}} {
		encoded, err := json.Marshal(liveOpFrom(op))
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if _, present := wire["branch"]; present {
			t.Fatalf("root operation wire changed: %s", encoded)
		}
	}
}
