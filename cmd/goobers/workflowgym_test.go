package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflowgym"
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

func TestWorkflowGymTournamentEmitsPairedReport(t *testing.T) {
	dataset := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "valid")
	registry, err := workflowgym.Load(dataset)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := writeTournamentCandidate(t, registry, "authoritative", []workflowgym.TournamentCandidateResult{
		{TaskID: "task-alpha", Status: workflowgym.TournamentPass},
		{TaskID: "task-beta", Status: workflowgym.TournamentPass},
	})
	candidate := writeTournamentCandidate(t, registry, "candidate", []workflowgym.TournamentCandidateResult{
		{TaskID: "task-alpha", Status: workflowgym.TournamentFail, FailureClass: "evaluation"},
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "tournament", dataset, authoritative, candidate}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var report workflowgym.TournamentReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode tournament report: %v\n%s", err, stdout.String())
	}
	if report.Kind != workflowgym.TournamentReportKind || len(report.Results) != 2 ||
		report.Results[1].CandidateRun.Status != workflowgym.TournamentFail ||
		report.Results[1].CandidateRun.FailureClass != "missing-result" {
		t.Fatalf("report = %+v", report)
	}
}

func TestWorkflowGymEvaluateEmitsMachineReadableFailGate(t *testing.T) {
	dataset := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "valid")
	registry, err := workflowgym.Load(dataset)
	if err != nil {
		t.Fatal(err)
	}
	authoritativePath := writeTournamentCandidate(t, registry, "authoritative", []workflowgym.TournamentCandidateResult{
		{TaskID: "task-alpha", Status: workflowgym.TournamentPass},
		{TaskID: "task-beta", Status: workflowgym.TournamentPass},
	})
	candidatePath := writeTournamentCandidate(t, registry, "candidate", []workflowgym.TournamentCandidateResult{
		{TaskID: "task-alpha", Status: workflowgym.TournamentFail, FailureClass: "evaluation"},
		{TaskID: "task-beta", Status: workflowgym.TournamentPass},
	})
	authoritative, err := workflowgym.LoadTournamentCandidate(authoritativePath)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := workflowgym.LoadTournamentCandidate(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	tournament, err := workflowgym.RunTournament(registry, authoritative, []workflowgym.TournamentCandidateManifest{candidate})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tournament)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "tournament.json")
	if err := os.WriteFile(reportPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"workflow-gym", "evaluate",
		"--min-samples", "1",
		"--regression-threshold", "0.25",
		dataset, reportPath,
	}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	var report workflowgym.EvaluationReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode evaluation report: %v\n%s", err, stdout.String())
	}
	if report.Gate.Verdict != workflowgym.EvaluationFail ||
		report.Digests.Dataset != registry.Digest ||
		len(report.Leaderboard) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func writeTournamentCandidate(t *testing.T, registry workflowgym.Registry, genome string, results []workflowgym.TournamentCandidateResult) string {
	t.Helper()
	for i := range results {
		for _, task := range registry.Tasks {
			if task.TaskID == results[i].TaskID {
				results[i].EvaluatorVersion = task.EvaluatorVersion
			}
		}
	}
	manifest := workflowgym.TournamentCandidateManifest{
		APIVersion:    apiv1.WorkflowGymTaskAPIVersion,
		Kind:          workflowgym.TournamentCandidateKind,
		DatasetDigest: registry.Digest,
		Identity: workflowgym.TournamentCandidateIdentity{
			EffectiveVersion: rollup.EffectiveVersion{
				WorkflowDigest: "sha256:workflow-" + genome,
				GooberDigest:   "sha256:goober-" + genome,
				Model:          "model-" + genome,
				HarnessVersion: "harness-1",
			},
			GenomeDigest: "sha256:" + genome,
		},
		Results: results,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), genome+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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
