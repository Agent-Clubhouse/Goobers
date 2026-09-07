package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

func TestArtifactSetsFollowRunRetentionIncludingFailedPublication(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	layout := instance.NewLayout(t.TempDir())
	if err := layout.EnsureGaggleRuntime("example"); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	type retainedSet struct {
		dir      string
		pointers []apiv1.ContextPointer
		orphan   apiv1.ArtifactPointer
	}
	sets := map[string]retainedSet{}
	for _, tc := range []struct {
		id       string
		age      time.Duration
		terminal bool
	}{
		{"expired", 48 * time.Hour, true},
		{"live", 48 * time.Hour, false},
		{"recent", time.Hour, true},
	} {
		run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
			RunID: tc.id, Workflow: "investigation", WorkflowVersion: 1, Gaggle: "example", Trigger: journal.Trigger{Kind: journal.TriggerManual},
		}, nil, journal.WithClock(func() time.Time { return now.Add(-tc.age) }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = run.Close() })
		pointers, orphan := publishRetentionSet(t, run)
		sets[tc.id] = retainedSet{dir: run.Dir(), pointers: pointers, orphan: orphan}
		if tc.terminal {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if err := db.IngestRun(ctx, run.Dir()); err != nil {
			t.Fatal(err)
		}
		assertRetainedSet(t, run.Dir(), pointers, orphan)
	}
	policy := Policy{Window: 24 * time.Hour, MaxRuns: 100}
	preview, err := Prune(layout, db, policy, Options{Now: now, DryRun: true})
	if err != nil || len(preview) != 1 || preview[0].RunID != "expired" {
		t.Fatalf("retention preview = %+v, %v", preview, err)
	}
	for _, set := range sets {
		assertRetainedSet(t, set.dir, set.pointers, set.orphan)
	}
	results, err := Prune(layout, db, policy, Options{Now: now})
	if err != nil || len(results) != 1 || results[0].RunID != "expired" {
		t.Fatalf("retention = %+v, %v", results, err)
	}
	if _, err := os.Stat(results[0].RunDir); !os.IsNotExist(err) {
		t.Fatalf("expired evidence/index/orphan root remains: %v", err)
	}
	for _, id := range []string{"live", "recent"} {
		set := sets[id]
		assertRetainedSet(t, set.dir, set.pointers, set.orphan)
	}
	assertRollupRunIDs(t, db, "live", "recent")
}

func publishRetentionSet(t *testing.T, run *journal.Run) ([]apiv1.ContextPointer, apiv1.ArtifactPointer) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("causal evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion, Entries: []artifactset.ManifestEntry{{Name: "evidence", Path: "evidence.txt", MediaType: "text/plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := artifactset.Prepare(context.Background(), workspace, "manifest.json", artifactset.NewSanitizer(journal.NewRegistryScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	record := func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := run.RecordArtifact(name, data)
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity, MediaType: media}, err
	}
	published, err := prepared.Publish(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	contexts := make([]apiv1.ContextPointer, len(published))
	for i := range published {
		contexts[i] = apiv1.ContextPointer{Name: fmt.Sprintf("instrument.artifact[%d]", i), Artifact: &published[i]}
	}
	// A later attempt records a distinct blob but fails before it can publish
	// an index. It must remain inside the same run's retention boundary.
	var orphan apiv1.ArtifactPointer
	failure := errors.New("injected storage failure")
	partial, err := prepared.Publish(context.Background(), func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		var writeErr error
		orphan, writeErr = record("failed-attempt", media, []byte("unreferenced attempted evidence"))
		return apiv1.ArtifactPointer{}, errors.Join(failure, writeErr)
	})
	if partial != nil || !errors.Is(err, failure) || orphan.Digest == "" {
		t.Fatalf("failed publication = %+v, %v", partial, err)
	}
	return contexts, orphan
}

func assertRetainedSet(t *testing.T, dir string, pointers []apiv1.ContextPointer, orphan apiv1.ArtifactPointer) {
	t.Helper()
	reader, err := artifactset.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	payloads, err := artifactset.Resolve(context.Background(), reader, pointers, "instrument", "evidence")
	if err != nil || string(payloads["evidence"].Bytes) != "causal evidence" {
		t.Fatalf("published set unreadable: %v", err)
	}
	if _, err := reader.ReadArtifact(context.Background(), orphan, artifactset.MaxPayloadBytes); err != nil {
		t.Fatalf("failed-attempt blob not retained: %v", err)
	}
}
