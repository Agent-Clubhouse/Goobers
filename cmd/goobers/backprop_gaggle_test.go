package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

func TestTelemetryGaggleRequiresGaggleAndToleratesEmptyInstance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runTelemetryGaggle([]string{t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d without --gaggle, want 2 (stderr %q)", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryGaggle([]string{"--gaggle=web", t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gaggle web: 0 active Backprop run(s)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

type telemetryGaggleFixture struct {
	runID, workflow, continuedFrom string
	backprop                       map[string]any
	phase                          journal.RunPhase
	sign                           float64
	labels                         []creditgraph.LabelOutcome
	startedAt                      time.Time
}

func writeTelemetryGaggleRun(t *testing.T, root string, store *readmodel.Store, fixture telemetryGaggleFixture) {
	t.Helper()
	layout := instance.NewLayout(root)
	definition, err := json.Marshal(map[string]any{
		"Name": fixture.workflow, "Version": 1, "dslVersion": "3.0",
		"Spec": map[string]any{"backprop": fixture.backprop},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: fixture.runID, Workflow: fixture.workflow, WorkflowVersion: 1, Gaggle: "web",
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, StartedAt: fixture.startedAt,
		ContinuedFromRunID: fixture.continuedFrom,
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(creditgraph.RunRecord{
		Schema: creditgraph.RecordSchemaVersion, Status: creditgraph.RecordComplete,
		RunID: fixture.runID, Gaggle: "web", Workflow: fixture.workflow, EffectiveVersion: "v1", Workload: "issue",
		Attribution: creditgraph.Attribution{OutcomeSign: fixture.sign, Contributions: []creditgraph.Contribution{{
			NodeID: "stage:act", Kind: creditgraph.KindStage, Stage: "act", Share: 1,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir(), creditgraph.RecordFileName), record, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(fixture.labels) > 0 {
		labels := creditgraph.LabelStore{Schema: creditgraph.LabelStoreSchemaVersion, RunID: fixture.runID}
		for i, outcome := range fixture.labels {
			at := fixture.startedAt.Add(time.Duration(i+1) * time.Hour)
			labels.Labels = append(labels.Labels, creditgraph.GroundTruthLabel{
				ID: fixture.runID + "-label-" + string(outcome), RunID: fixture.runID, Outcome: outcome,
				Source: creditgraph.LabelSourceHuman, LabeledBy: "reviewer", LabeledAt: at, RecordedAt: at,
			})
		}
		data, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run.Dir(), creditgraph.LabelStoreFileName), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	finished := fixture.startedAt.Add(time.Minute)
	if err := store.UpsertRun(context.Background(), readmodel.Projection{Run: readmodel.RunRow{
		RunID: fixture.runID, Gaggle: "web", Workflow: fixture.workflow, TriggerKind: string(journal.TriggerManual),
		Phase: fixture.phase, Terminal: true, StartedAt: fixture.startedAt, FinishedAt: &finished, LastActivity: finished,
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryGaggleJSONReportsPopulatedGaggle(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := readmodel.Open(layout.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	humanOnly := map[string]any{"mode": "active", "version": "v1", "groundTruth": map[string]any{"sources": []string{"human"}}}
	optOut := map[string]any{"mode": "active", "version": "v1", "groundTruth": map[string]any{"sources": []string{}}}
	for _, fixture := range []telemetryGaggleFixture{
		{runID: "impl-1", workflow: "implement", backprop: humanOnly, phase: journal.PhaseCompleted, sign: 1,
			labels: []creditgraph.LabelOutcome{creditgraph.LabelCorrect, creditgraph.LabelIncorrect}, startedAt: start},
		{runID: "impl-2", workflow: "implement", backprop: humanOnly, phase: journal.PhaseCompleted, sign: 1,
			labels: []creditgraph.LabelOutcome{creditgraph.LabelCorrect}, startedAt: start.Add(time.Hour)},
		{runID: "review-1", workflow: "review", backprop: optOut, phase: journal.PhaseFailed, sign: -1,
			labels: []creditgraph.LabelOutcome{creditgraph.LabelIncorrect}, continuedFrom: "impl-2", startedAt: start.Add(2 * time.Hour)},
	} {
		writeTelemetryGaggleRun(t, root, store, fixture)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"--gaggle=web", "--json", "--since=2026-10-08T00:00:00Z", "--until=2026-10-09T00:00:00Z", root}
	if code := runTelemetryGaggle(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	var report struct {
		Schema    string `json:"schema"`
		Gaggle    string `json:"gaggle"`
		Runs      int    `json:"runs"`
		Workflows []struct {
			Workflow           string                          `json:"workflow"`
			Runs               int                             `json:"runs"`
			Failed             int                             `json:"failed"`
			CreditShare        float64                         `json:"creditShare"`
			BlameShare         float64                         `json:"blameShare"`
			DownstreamFailures int                             `json:"downstreamFailures"`
			GroundTruthSources []string                        `json:"groundTruthSources"`
			GroundTruth        *creditgraph.GroundTruthSummary `json:"groundTruth"`
			IgnoredLabelRuns   int                             `json:"ignoredLabelRuns"`
		} `json:"workflows"`
		Links []struct {
			Basis        string `json:"basis"`
			Identifier   string `json:"identifier"`
			FromRunID    string `json:"fromRunId"`
			FromWorkflow string `json:"fromWorkflow"`
			ToRunID      string `json:"toRunId"`
			ToWorkflow   string `json:"toWorkflow"`
			ToPhase      string `json:"toPhase"`
		} `json:"links"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if report.Schema != readservice.GaggleBackpropReportSchema || report.Gaggle != "web" || report.Runs != 3 {
		t.Fatalf("report header = schema %q gaggle %q runs %d", report.Schema, report.Gaggle, report.Runs)
	}
	if len(report.Workflows) != 2 {
		t.Fatalf("workflows = %+v, want implement and review", report.Workflows)
	}
	implement, review := report.Workflows[0], report.Workflows[1]
	if implement.Workflow != "implement" || implement.Runs != 2 || implement.CreditShare != 1 || implement.BlameShare != 0 ||
		implement.DownstreamFailures != 1 {
		t.Fatalf("implement = %+v, want all credit and one downstream failure", implement)
	}
	// impl-1's latest label is incorrect; impl-2's is correct.
	if want := (creditgraph.GroundTruthSummary{LabeledRunCount: 2, CorrectRunCount: 1, IncorrectRunCount: 1}); implement.GroundTruth == nil ||
		*implement.GroundTruth != want || len(implement.GroundTruthSources) != 1 || implement.GroundTruthSources[0] != "human" {
		t.Fatalf("implement ground truth = %+v sources %v, want %+v from human", implement.GroundTruth, implement.GroundTruthSources, want)
	}
	if review.Workflow != "review" || review.Failed != 1 || review.BlameShare != 1 {
		t.Fatalf("review = %+v, want the only blame", review)
	}
	// review opted out of ground truth: its human label is reported as
	// ignored and never summarized.
	if review.GroundTruth != nil || review.GroundTruthSources == nil || len(review.GroundTruthSources) != 0 || review.IgnoredLabelRuns != 1 {
		t.Fatalf("review ground truth = %+v sources %v ignored %d, want opted out", review.GroundTruth, review.GroundTruthSources, review.IgnoredLabelRuns)
	}
	if len(report.Links) != 1 {
		t.Fatalf("links = %+v, want the one cross-workflow continuation", report.Links)
	}
	if link := report.Links[0]; link.Basis != readservice.GaggleLinkContinuedFrom || link.Identifier != "impl-2" ||
		link.FromRunID != "impl-2" || link.FromWorkflow != "implement" || link.ToRunID != "review-1" ||
		link.ToWorkflow != "review" || link.ToPhase != string(journal.PhaseFailed) {
		t.Fatalf("link = %+v", link)
	}
}
