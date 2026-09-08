package readmodel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/prqueue"
)

func TestQueueEligibilityRealJournalProjectionAndTamperDetection(t *testing.T) {
	identity, event, fixture := queueProjectionFixture(t)
	root := t.TempDir()
	run, err := journal.Create(filepath.Join(root, "runs"), identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	ref, err := run.RecordArtifact(event.Stage+"/result", fixture.blobs[event.Artifacts[0].Digest])
	if err != nil {
		t.Fatal(err)
	}
	event.Artifacts = []journal.Ref{ref}
	event.Status = "success"
	event.Attempt = 1
	event.Seq = 0
	if err := run.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "runs", identity.RunID)
	store, err := Open(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ProjectRunDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	row, found, err := store.GetRun(context.Background(), identity.RunID)
	if err != nil || !found {
		t.Fatalf("projected row: found=%t err=%v", found, err)
	}
	if row.Operator.QueueEligibility == nil || row.Operator.QueueEligibility.Problem != "" || row.Operator.QueueEligibility.Artifact.Digest != ref.Digest {
		t.Fatalf("real projection lost queue artifact: %+v", row.Operator.QueueEligibility)
	}
	if err := os.WriteFile(filepath.Join(dir, ref.Path), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := projectQueueEligibility(reader, identity, []journal.Event{event})
	if got == nil || got.Problem == "" || got.Artifact != nil {
		t.Fatalf("tampered blob trusted: %+v", got)
	}
}

type queueBlobFixture struct {
	blobs map[string][]byte
	reads int
	bytes int64
}

func (f *queueBlobFixture) ArtifactBytesBounded(ref journal.Ref, limit int64) ([]byte, error) {
	f.reads++
	data, ok := f.blobs[ref.Digest]
	if !ok || int64(len(data)) > limit {
		return nil, errors.New("missing or oversized blob")
	}
	f.bytes += int64(len(data))
	return data, nil
}

func queueProjectionFixture(t *testing.T) (journal.RunIdentity, journal.Event, *queueBlobFixture) {
	t.Helper()
	identity := journal.RunIdentity{RunID: "run", Workflow: "review", Gaggle: "team"}
	report := prqueue.Report{Version: 1, RepositoryKey: "github|||org|repo|", Workflow: identity.Workflow, Gaggle: identity.Gaggle, RunID: identity.RunID, ObservedAt: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), Items: []prqueue.Item{}}
	report.Add(42, prqueue.Escalated)
	data, err := json.Marshal(map[string]any{"queueEligibility": report})
	if err != nil {
		t.Fatal(err)
	}
	ref := journal.Ref{Digest: journal.Digest(data), Path: "artifacts/report", Size: int64(len(data))}
	event := journal.Event{Schema: journal.EventSchema, Type: journal.EventStageFinished, Stage: "arbitrarily-named-selector", Seq: 10, Outputs: map[string]any{"queueEligibilityVersion": "1"}, Artifacts: []journal.Ref{ref}}
	return identity, event, &queueBlobFixture{blobs: map[string][]byte{ref.Digest: data}}
}

func TestQueueEligibilityProjectsCompactScopedEvidence(t *testing.T) {
	identity, event, reader := queueProjectionFixture(t)
	got := projectQueueEligibility(reader, identity, []journal.Event{event})
	if got == nil || got.Problem != "" || got.Artifact == nil || got.MatchingItems != 1 || got.Stage != event.Stage || got.RepositoryKey != "github|||org|repo|" || got.CompleteSnapshot {
		t.Fatalf("lost or misrepresented evidence: %+v", got)
	}
	if reader.reads != 1 {
		t.Fatalf("reads=%d", reader.reads)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "nextStep") || len(encoded) > 1024 {
		t.Fatalf("list metadata embedded report payload: %s", encoded)
	}
}

func TestQueueEligibilityDoesNotFallBackAcrossInvalidLatestEvidence(t *testing.T) {
	for _, problem := range []string{"missing", "invalid", "wrong-owner", "unknown-version"} {
		t.Run(problem, func(t *testing.T) {
			identity, old, reader := queueProjectionFixture(t)
			latest := old
			latest.Seq++
			latest.Artifacts = []journal.Ref{{Digest: "latest"}}
			switch problem {
			case "invalid":
				reader.blobs["latest"] = []byte(`{"queueEligibility":{}}`)
			case "wrong-owner":
				reader.blobs["latest"] = []byte(strings.Replace(string(reader.blobs[old.Artifacts[0].Digest]), `"runId":"run"`, `"runId":"other"`, 1))
			case "unknown-version":
				latest.Outputs = map[string]any{"queueEligibilityVersion": "99"}
			}
			got := projectQueueEligibility(reader, identity, []journal.Event{old, latest})
			if got == nil || got.Seq != latest.Seq || got.Problem == "" || got.Artifact != nil {
				t.Fatalf("fell back to misleading evidence: %+v", got)
			}
		})
	}
}

func TestQueueEligibilityArtifactDiscoveryHasCountAndByteBounds(t *testing.T) {
	identity, event, reader := queueProjectionFixture(t)
	reader.blobs["other"] = []byte(`{}`)
	for range 100 {
		event.Artifacts = append(event.Artifacts, journal.Ref{Digest: "other"})
	}
	got := projectQueueEligibility(reader, identity, []journal.Event{event})
	if got == nil || got.Problem == "" || reader.reads != 16 {
		t.Fatalf("unbounded discovery: reads=%d evidence=%+v", reader.reads, got)
	}
	reader.reads = 0
	reader.blobs["other"] = []byte(strings.Repeat("x", prqueue.MaxReportBytes+65537))
	got = projectQueueEligibility(reader, identity, []journal.Event{event})
	if got == nil || got.Problem == "" || reader.reads != 1 {
		t.Fatalf("oversized blob accepted: reads=%d evidence=%+v", reader.reads, got)
	}
}
