package rollup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLearningEpisodesProjectAndClusterNonPassReview(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "runs", "learning-run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "run.yaml"), []byte(`
schema: goobers.dev/journal/run/v1
runId: learning-run
workflow: implementation
workflowVersion: 1
workflowDigest: sha256:workflow
gooberDigest: sha256:goober
gaggle: web
startedAt: 2026-08-21T00:00:00Z
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2026-08-21T00:00:00Z","type":"run.started"}
{"schema":"goobers.dev/journal/event/v1","seq":2,"time":"2026-08-21T00:00:01Z","type":"gate.evaluated","gate":"review","verdict":"needs-changes","target":"implement","runner":{"failureSignature":"missing-test","correctionFeedback":"Add regression coverage.","findingIdentities":["finding-1"]}}
{"schema":"goobers.dev/journal/event/v1","seq":3,"time":"2026-08-21T00:00:02Z","type":"stage.started","stage":"implement","attempt":2}
{"schema":"goobers.dev/journal/event/v1","seq":4,"time":"2026-08-21T00:00:03Z","type":"gate.evaluated","gate":"review","verdict":"pass"}
{"schema":"goobers.dev/journal/event/v1","seq":5,"time":"2026-08-21T00:00:04Z","type":"run.finished","status":"completed"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.IngestRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{Gaggle: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	episode := episodes[0]
	if episode.Signature != "review|needs-changes|missing-test" ||
		episode.NextAttempt != 2 || episode.Outcome != "fixed" ||
		episode.EffectiveVersion == "" || len(episode.FindingIdentities) != 1 ||
		episode.FindingIdentities[0] != "finding-1" {
		t.Fatalf("episode = %+v", episode)
	}
	clusters, err := db.LearningClusters(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Count != 1 ||
		clusters[0].RecommendedAction != "instruction-or-skill" {
		t.Fatalf("clusters = %+v", clusters)
	}
	if clusters[0].Episodes[0] != (JournalPointer{RunID: "learning-run", Seq: 2}) {
		t.Fatalf("cluster evidence = %+v, want source sequence", clusters[0].Episodes)
	}
}

func TestLearningEpisodesProjectValidationFailure(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "runs", "validation-run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "run.yaml"), []byte(`
schema: goobers.dev/journal/run/v1
runId: validation-run
workflow: implementation
workflowVersion: 1
gaggle: web
startedAt: 2026-08-21T00:00:00Z
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(`{"schema":"goobers.dev/journal/event/v1","seq":1,"time":"2026-08-21T00:00:00Z","type":"stage.started","stage":"local-ci","attempt":1}
{"schema":"goobers.dev/journal/event/v1","seq":2,"time":"2026-08-21T00:00:01Z","type":"stage.finished","stage":"local-ci","attempt":1,"status":"failure","error":{"code":"test_failed","message":"targeted test failed"}}
{"schema":"goobers.dev/journal/event/v1","seq":3,"time":"2026-08-21T00:00:02Z","type":"stage.started","stage":"local-ci","attempt":2}
{"schema":"goobers.dev/journal/event/v1","seq":4,"time":"2026-08-21T00:00:03Z","type":"stage.finished","stage":"local-ci","attempt":2,"status":"success"}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.IngestRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	episodes, err := db.LearningEpisodes(context.Background(), LearningEpisodeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 {
		t.Fatalf("episodes = %d, want 1", len(episodes))
	}
	if episodes[0].Signature != "local-ci|failure|test_failed" ||
		episodes[0].NextAttempt != 2 || episodes[0].Outcome != "fixed" {
		t.Fatalf("episode = %+v", episodes[0])
	}
}
