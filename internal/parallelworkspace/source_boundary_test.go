package parallelworkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
)

func TestVerifySourceBoundaryUsesSuppliedEvents(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	meta := sourceMetadata{Parallel: "fan", Sequence: 3, Snapshot: recovery.ChildSnapshot{Record: recovery.Record{CreatedAt: at}}}
	good := journal.Event{Seq: 3, Type: journal.EventParallelStarted, Parallel: "fan", Time: at}
	if err := verifySourceBoundary([]journal.Event{{Seq: 2}, good}, meta); err != nil {
		t.Fatal(err)
	}
	if err := verifySourceBoundary([]journal.Event{{Seq: 2}}, meta); err == nil {
		t.Fatal("missing boundary accepted")
	}
	bad := good
	bad.Parallel = "other"
	if err := verifySourceBoundary([]journal.Event{bad}, meta); err == nil {
		t.Fatal("changed boundary accepted")
	}
}

func TestPreparationReplayReadsJournalOnce(t *testing.T) {
	at := time.Unix(1000, 0).UTC()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: testRun, Gaggle: testGaggle}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "o", Name: "r"}
	const count = 4
	for i := 0; i < count; i++ {
		if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: testPar}); err != nil {
			t.Fatal(err)
		}
		started, err := reader(t, run).Events()
		if err != nil {
			t.Fatal(err)
		}
		sha := strings.Repeat(string(rune('a'+i)), 40)
		ref, err := recovery.RefForSnapshot(testRun, sha)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := recovery.ChildSnapshot{Record: recovery.Record{
			Version: 1, RunID: testRun, Ref: ref, RepositoryKey: repositoryKey(repo),
			BaseSHA: strings.Repeat("0", 40), SnapshotSHA: sha, PatchDigest: "sha256:" + strings.Repeat("1", 64),
			CreatedAt: at, RetainUntil: at.Add(time.Hour),
		}}
		request := spec.ResultRequest{Request: spec.Request{RunID: testRun, Gaggle: testGaggle, Parallel: testPar, Sequence: started[len(started)-1].Seq, At: at, Repository: repo}}
		if err := recordPreparation(run, request, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	r := reader(t, run)
	id, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := r.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.Dir(), "run.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.Dir(), "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	reads := 0
	pending, total, err := replayPreparations(events, func(event journal.Event) (PendingPreparation, error) {
		reads++
		return readPreparation(r, id, events, event)
	})
	if err != nil {
		t.Fatalf("replay re-read run.yaml or events per preparation: %v", err)
	}
	if len(pending) != count || total != count || reads != count {
		t.Fatalf("pending=%d total=%d reads=%d, want %d", len(pending), total, reads, count)
	}
}
