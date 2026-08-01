package readmodel

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The measurement merge (#1782).
//
// These tests are about the two ways the flags can be wrong in a way no list
// query would reveal: silently cleared, and silently widened.

// stubMeasurement is a MeasurementSource with a scripted answer.
type stubMeasurement struct {
	byRun map[string][]StageMeasurement
	err   error
	calls int
}

func (s *stubMeasurement) RunMeasurement(_ context.Context, runID string) ([]StageMeasurement, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.byRun[runID], nil
}

// projectionWithStages builds a projection carrying the named stages.
func projectionWithStages(runID string, stages ...string) Projection {
	p := Projection{Run: RunRow{
		RunID:     runID,
		Gaggle:    "alpha",
		Workflow:  "wf",
		StartedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		LastSeq:   1,
		Stages:    stages,
	}}
	for _, stage := range stages {
		p.Stages = append(p.Stages, StageRow{RunID: runID, Stage: stage, Attempts: 1})
	}
	return p
}

// TestMeasurementRollupIsTheOrAcrossStages pins that the run-level flags cannot
// disagree with the stage-level ones.
//
// The two grains answer different questions — `population=cost-measured` with a
// stage, and without one — and they are stored separately. If the rollup were
// computed in SQL, or maintained by hand, a run could be flagged at run level
// with no stage justifying it, and the unscoped filter would return a run the
// scoped filter says has nothing.
func TestMeasurementRollupIsTheOrAcrossStages(t *testing.T) {
	p := projectionWithStages("run-a", "build", "test")
	p.ApplyMeasurement([]StageMeasurement{
		{Stage: "build", CostMeasured: true},
		{Stage: "test", TokenMeasured: true},
	})

	if !p.Run.AnyCostMeasured || !p.Run.AnyTokenMeasured {
		t.Errorf("rollup missed a stage: cost=%v token=%v",
			p.Run.AnyCostMeasured, p.Run.AnyTokenMeasured)
	}
	if p.Run.AnyPremiumMeasured || p.Run.AnyRetryWaste {
		t.Error("rollup set a flag no stage carries")
	}
	// And the scoped answer must stay scoped: build has cost, test does not.
	for _, stage := range p.Stages {
		if stage.Stage == "test" && stage.CostMeasured {
			t.Error("cost leaked from build onto test; the scoped filter would widen")
		}
	}
}

// TestMeasurementIsClearedWhenTelemetrySpeaks pins the difference between "no
// measurements" and "nothing to say".
func TestMeasurementIsClearedWhenTelemetrySpeaks(t *testing.T) {
	p := projectionWithStages("run-a", "build")
	p.ApplyMeasurement([]StageMeasurement{{Stage: "build", CostMeasured: true}})
	if !p.Run.AnyCostMeasured {
		t.Fatal("setup did not set the flag")
	}

	// An empty NON-nil slice is a positive statement: this run has no measured
	// stages. It clears.
	p.ApplyMeasurement([]StageMeasurement{})
	if p.Run.AnyCostMeasured || p.Stages[0].CostMeasured {
		t.Error("an empty measurement did not clear the flags")
	}
}

// TestNilMeasurementPreservesCarriedFlags is the other half, and the one that
// matters operationally: a telemetry outage must not empty a population filter.
func TestNilMeasurementPreservesCarriedFlags(t *testing.T) {
	p := projectionWithStages("run-a", "build")
	p.ApplyMeasurement([]StageMeasurement{{Stage: "build", RetryWaste: true}})

	p.ApplyMeasurement(nil)
	if !p.Stages[0].RetryWaste || !p.Run.AnyRetryWaste {
		t.Error("a nil measurement cleared flags; a telemetry outage would silently " +
			"empty every population filter until each run changed again")
	}
}

// TestMeasurementSourceErrorDoesNotClearFlags carries the same property through
// the Store seam, where the decision is actually made.
func TestMeasurementSourceErrorDoesNotClearFlags(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	healthy := &stubMeasurement{byRun: map[string][]StageMeasurement{
		"run-a": {{Stage: "build", CostMeasured: true}},
	}}
	store.WithMeasurement(healthy)
	if err := store.UpsertRun(ctx, projectionWithStages("run-a", "build")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row, ok, err := store.GetRun(ctx, "run-a")
	if err != nil || !ok {
		t.Fatalf("get run: %v ok=%v", err, ok)
	}
	if !row.AnyCostMeasured {
		t.Fatal("the healthy projection did not store the flag")
	}

	// Telemetry now fails. Re-projecting at a newer sequence must not clear it.
	broken := &stubMeasurement{err: errors.New("telemetry is down")}
	store.WithMeasurement(broken)
	next := projectionWithStages("run-a", "build")
	next.Run.LastSeq = 2
	// The caller carries the prior flags forward, which is what ProjectRun's
	// carryStages does on the real path.
	next.Stages[0].CostMeasured = true
	if err := store.UpsertRun(ctx, next); err != nil {
		t.Fatalf("upsert during outage: %v", err)
	}
	row, _, err = store.GetRun(ctx, "run-a")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !row.AnyCostMeasured {
		t.Error("a telemetry error cleared a stored measurement flag")
	}
	if broken.calls == 0 {
		t.Error("the measurement source was never consulted")
	}
}

// TestStageScopedPopulationDoesNotMatchOtherStages is the widening bug, checked
// through a real query rather than through the merge.
//
// A run-level rollup used for a stage-scoped filter would return this run for
// `stage=test&population=cost-measured`, because the RUN has cost — in `build`.
func TestStageScopedPopulationDoesNotMatchOtherStages(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	p := projectionWithStages("run-a", "build", "test")
	p.ApplyMeasurement([]StageMeasurement{{Stage: "build", CostMeasured: true}})
	if err := store.UpsertRun(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	build, err := store.ListRuns(ctx, ListOptions{Stage: "build", Population: PopulationCostMeasured})
	if err != nil {
		t.Fatalf("list build: %v", err)
	}
	if len(build.Runs) != 1 {
		t.Errorf("stage=build&population=cost-measured returned %d runs, want 1", len(build.Runs))
	}

	test, err := store.ListRuns(ctx, ListOptions{Stage: "test", Population: PopulationCostMeasured})
	if err != nil {
		t.Fatalf("list test: %v", err)
	}
	if len(test.Runs) != 0 {
		t.Errorf("stage=test&population=cost-measured returned %d runs; the run's cost is in "+
			"`build`, so a stage-scoped filter answered from the run-level rollup has widened",
			len(test.Runs))
	}

	// The unscoped form asks a different question and must still match.
	any, err := store.ListRuns(ctx, ListOptions{Population: PopulationCostMeasured})
	if err != nil {
		t.Fatalf("list unscoped: %v", err)
	}
	if len(any.Runs) != 1 {
		t.Errorf("population=cost-measured returned %d runs, want 1", len(any.Runs))
	}
}

// TestStageFilterOrdersByRunRecencyNotStageRecency pins why run_started_at is
// duplicated onto run_stage.
//
// run_stage.started_at is the STAGE's start. Ordering a stage-filtered list by
// it would interleave runs by when their build stage happened to begin, which is
// not the order the page claims — and, worse, would need a sort, so the page
// would stop being bounded.
func TestStageFilterOrdersByRunRecencyNotStageRecency(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Older run whose `build` stage started LATER than the newer run's.
	older := projectionWithStages("run-old", "build")
	older.Run.StartedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	lateStage := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	older.Stages[0].StartedAt = &lateStage

	newer := projectionWithStages("run-new", "build")
	newer.Run.StartedAt = time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	earlyStage := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	newer.Stages[0].StartedAt = &earlyStage

	for _, p := range []Projection{older, newer} {
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatalf("upsert %s: %v", p.Run.RunID, err)
		}
	}

	page, err := store.ListRuns(ctx, ListOptions{Stage: "build"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(page.Runs))
	}
	if page.Runs[0].RunID != "run-new" {
		t.Errorf("first row = %q, want run-new; the page is ordered by STAGE recency "+
			"rather than run recency", page.Runs[0].RunID)
	}
}

// TestStageFilterIsGaggleScoped pins §5.5: gaggle must be a query predicate on
// the stage-scoped path too, not a post-filter.
func TestStageFilterIsGaggleScoped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	mine := projectionWithStages("run-mine", "build")
	theirs := projectionWithStages("run-theirs", "build")
	theirs.Run.Gaggle = "beta"
	for _, p := range []Projection{mine, theirs} {
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatalf("upsert %s: %v", p.Run.RunID, err)
		}
	}

	page, err := store.ListRuns(ctx, ListOptions{Gaggle: "alpha", Stage: "build"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Runs) != 1 || page.Runs[0].RunID != "run-mine" {
		t.Errorf("gaggle+stage returned %d runs (%v); a gaggle-scoped principal must not "+
			"see another gaggle's runs through a stage filter", len(page.Runs), page.Runs)
	}
}
