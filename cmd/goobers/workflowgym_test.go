package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

func TestWorkflowGymListPrintsDigestCountAndStableOrder(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "valid")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "list", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"datasetVersion: 2026.1", "datasetDigest: sha256:", "taskCount: 2"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Index(output, "task-alpha") > strings.Index(output, "task-beta") {
		t.Fatalf("tasks not sorted by ID:\n%s", output)
	}
}

func TestWorkflowGymValidateRejectsInvalidFixture(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "missing-provenance")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "validate", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "provenance") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestWorkflowGymReplayEmitsOfflineManifest(t *testing.T) {
	task := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "valid", "10-train.yaml")
	providerFixture := filepath.Join("..", "..", "internal", "harness", "testdata", "workflowgym", "provider.json")
	journalDir := workflowGymReplayJournal(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "replay", task, providerFixture, journalDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var manifest apiv1.WorkflowGymReplayManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode replay manifest: %v\n%s", err, stdout.String())
	}
	if manifest.Kind != apiv1.WorkflowGymReplayKind || manifest.Status != "success" ||
		manifest.JournalDigest == "" || len(manifest.NondeterminismFindings) != 0 {
		t.Fatalf("manifest = %+v, want deterministic replay success", manifest)
	}
	// The CLI executes the recorded session: it reads the fixture issue and
	// re-posts the journal's recorded comment through the virtual sink. The
	// fixture's list-open-issues exchange is never requested, so it is absent.
	var got []string
	for _, effect := range manifest.Effects {
		got = append(got, string(effect.Disposition)+" "+effect.Effect+" "+effect.Target)
	}
	want := []string{
		"deterministic artifact transcript.jsonl",
		"deterministic artifact completion.json",
		"virtualized provider-read GET /repos/acme/web/issues/7",
		"virtualized comment POST /repos/acme/web/issues/7/comments",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("effects = %q, want %q", got, want)
	}
}

func workflowGymReplayJournal(t *testing.T) string {
	t.Helper()
	runJournal, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "workflow-gym",
		WorkflowVersion: 1,
		Gaggle:          "test",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "task-alpha"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runJournal.Close() }()
	if err := runJournal.Append(journal.Event{Type: journal.EventStageStarted, Stage: "candidate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	completion, err := json.Marshal(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "historical result"})
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []struct {
		name string
		data []byte
	}{
		{harness.BenchmarkTranscriptArtifact, []byte("{\"message\":\"offline\"}\n")},
		{harness.BenchmarkCompletionArtifact, completion},
	} {
		if _, err := runJournal.RecordStageArtifact("candidate", 1, journal.AttemptPolicy, artifact.name, artifact.data); err != nil {
			t.Fatal(err)
		}
	}
	// Run-branch provenance, as every repo-using run records it: not a mutation.
	if err := runJournal.Append(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: "goobers/workflow-gym/0af76519", CommitSHA: "abc123"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runJournal.Append(journal.Event{
		Type: journal.EventRefTouched, Stage: "candidate", Attempt: 1,
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "7"},
		Runner:      map[string]any{"operation": "comment"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runJournal.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "candidate", Attempt: 1, Status: "success",
	}); err != nil {
		t.Fatal(err)
	}
	return runJournal.Dir()
}
