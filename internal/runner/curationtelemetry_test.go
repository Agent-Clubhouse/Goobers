package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/workflow"
)

// curationWorkflowStageNames pins the three stage names #2277's rollup
// projectors (internal/telemetry/rollup/ingest.go's insertCurationAction,
// insertReadyPoolSample, insertReadyLabelTransitions) hard-code, against the
// actual shipped reference workflow — so a future rename in
// reference-workflows/gaggles/goobers/workflows/backlog-curation.yaml (with a
// forgotten matching projector update, or vice versa) fails this test instead
// of silently going dark the way #2277 itself did.
func curationWorkflowStageNames(t *testing.T) (reconcile, sample, curate string) {
	t.Helper()
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "backlog-curation.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference backlog-curation workflow: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal reference backlog-curation workflow: %v", err)
	}
	var have struct{ reconcile, sample, curate bool }
	for _, task := range w.Spec.Tasks {
		switch task.Name {
		case "reconcile-backlog":
			have.reconcile = true
		case "sample-ready-pool":
			have.sample = true
		case "curate":
			have.curate = true
			if task.Goober != "curator" {
				t.Fatalf("curate task goober = %q, want curator", task.Goober)
			}
		}
	}
	if !have.reconcile || !have.sample || !have.curate {
		t.Fatalf("reference backlog-curation workflow tasks = %+v, want reconcile-backlog, sample-ready-pool, and curate all present", w.Spec.Tasks)
	}
	return "reconcile-backlog", "sample-ready-pool", "curate"
}

// curationOutcomeGoober is a fake invoke.Goober for the curate stage: it
// journals the seven agent-owned scalar outputs (ingest.go's
// curationAgentOutputKeys) the shipped curator/instructions.md requires every
// successful curation run to set.
type curationOutcomeGoober struct{}

func (*curationOutcomeGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Outputs: map[string]interface{}{
			"ready":      3,
			"needsHuman": 1,
			"closed":     2,
			"deduped":    1,
			"split":      0,
			"stale":      1,
			"milestoned": 1,
		},
	}, nil
}

func (*curationOutcomeGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errNoReviewer
}

var errNoReviewer = &curationTestError{"curationOutcomeGoober: Review not exercised by this test"}

type curationTestError struct{ msg string }

func (e *curationTestError) Error() string { return e.msg }

// TestBacklogCurationRunPersistsHealthTelemetry is #2277's end-to-end
// regression guard: the ready-pool/curation-action/ready-label-transition
// rollup projectors exist and are wired into IngestRun, but their guard
// conditions are keyed on hard-coded stage names and output shapes that
// nothing previously verified a real curation run actually produces — so the
// live telemetry store recorded zero rows despite the writers "existing".
// This drives a real `backlog-curation` run through the runner (real journal,
// real artifact recording, real event ordering — not a hand-authored
// events.jsonl like curation_test.go's projector-only tests) and asserts
// non-zero, correctly-shaped rows land in all three tables, while
// TestReadyClaimAgeAndDemandAreQueryable (internal/telemetry/rollup) keeps
// guarding that the already-working ready-claim path stays intact.
func TestBacklogCurationRunPersistsHealthTelemetry(t *testing.T) {
	reconcileStage, sampleStage, curateStage := curationWorkflowStageNames(t)

	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	observedAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	readyArtifact, err := json.Marshal(map[string]interface{}{
		"readyTransitions": []map[string]interface{}{
			{"eventId": 1, "itemId": "1001", "label": "goobers:ready", "added": true, "occurredAt": observedAt.Format(time.RFC3339Nano)},
			{"eventId": 2, "itemId": "1001", "label": "goobers:ready", "added": false, "occurredAt": observedAt.Add(time.Hour).Format(time.RFC3339Nano)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, err := telemetry.NewRunID()
	if err != nil {
		t.Fatal(err)
	}

	// stubDeterministic keys byTask on the full envelope TaskID, which the
	// runner mints as "<runID>:<stage name>" — not the bare stage name.
	det := &stubDeterministic{byTask: map[string]stubTaskResult{
		runID + ":" + reconcileStage: {
			status:  apiv1.ResultSuccess,
			outputs: map[string]interface{}{"reconciled": 2},
		},
		runID + ":" + sampleStage: {
			status: apiv1.ResultSuccess,
			outputs: map[string]interface{}{
				"readyPoolDepth":         5,
				"averageReadyAgeSeconds": 3600.0,
				"oldestReadyAgeSeconds":  7200.0,
				"readyPoolObservedAt":    observedAt.Format(time.RFC3339Nano),
			},
			artifactName:      "backlog-health.json",
			artifactData:      readyArtifact,
			artifactMediaType: "application/json",
		},
	}}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			det.rec = rec
			return det, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &curationOutcomeGoober{}, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	machine, err := workflow.Compile(workflow.Definition{
		Name:    "backlog-curation",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "goobers",
			Start:  reconcileStage,
			Tasks: []apiv1.Task{
				{
					Name: reconcileStage, Type: apiv1.TaskDeterministic, Goal: "reconcile drifted backlog labels",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
					Next: sampleStage,
				},
				{
					Name: sampleStage, Type: apiv1.TaskDeterministic, Goal: "snapshot ready-pool depth and age",
					Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
					Next: curateStage,
				},
				{
					Name: curateStage, Type: apiv1.TaskAgentic, Goober: "curator", Goal: "curate forward items",
					Next: workflow.TerminalComplete,
				},
			},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}

	result, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "goobers", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}

	db, err := rollup.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, runID)); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	// A single unfiltered request: readyPoolHealth zeroes itself out whenever
	// req.Workflow is non-empty (curation.go), so ReadyPool must be read off
	// the same unfiltered Stats call as Curation, not a Workflow-scoped one.
	statsResult, err := db.Stats(context.Background(), rollup.StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	stats := statsResult.Curation
	if stats.Runs != 1 {
		t.Fatalf("curation runs = %d, want 1 (curation_actions row missing or not counted)", stats.Runs)
	}
	if stats.ReportedRuns != 1 {
		t.Fatal("curation reportedRuns = 0, want 1 — all seven curate outputs were valid")
	}
	if stats.Ready != 3 || stats.NeedsHuman != 1 || stats.Closed != 2 || stats.Deduped != 1 ||
		stats.Split != 0 || stats.Stale != 1 || stats.Milestoned != 1 || stats.Reconciled != 2 {
		t.Fatalf("curation stats = %+v, want ready=3 needsHuman=1 closed=2 deduped=1 split=0 stale=1 milestoned=1 reconciled=2", stats)
	}

	health := statsResult.ReadyPool
	if !health.HasSample {
		t.Fatal("ready-pool health has no sample — ready_pool_samples row missing")
	}
	if health.Depth != 5 || health.AverageAgeSeconds != 3600 || health.OldestAgeSeconds != 7200 {
		t.Fatalf("ready-pool health = %+v, want depth=5 averageAgeSeconds=3600 oldestAgeSeconds=7200", health)
	}
	if !health.HasBounceRate {
		t.Fatal("ready-pool health has no bounce rate — ready_label_transitions rows missing")
	}
	if health.BounceRate != 1 {
		t.Fatalf("bounce rate = %v, want 1 (one bounce out of one ready cohort item in window)", health.BounceRate)
	}
}
