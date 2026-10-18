package readmodel

import (
	"context"
	"testing"
	"time"
)

func TestHarvestCalibrationCollectsEmpiricalCostObservations(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)

	withCost := projectionWithStages("run-with-cost", "check")
	withCost.Run.StartedAt = start
	withCost.Stages[0].StartedAt = &start
	withCost.Stages[0].FinishedAt = &end
	withCost.Stages[0].HadSuccess = true
	withCost.ApplyMeasurement([]StageMeasurement{{Stage: "check", CostMeasured: true}})
	if err := store.UpsertRun(ctx, withCost); err != nil {
		t.Fatalf("upsert run-with-cost: %v", err)
	}

	withoutCost := projectionWithStages("run-without-cost", "check")
	withoutCost.Run.StartedAt = start.Add(2 * time.Minute)
	withoutCost.Stages[0].StartedAt = &start
	withoutCost.Stages[0].FinishedAt = &end
	withoutCost.Stages[0].HadSuccess = true
	withoutCost.ApplyMeasurement([]StageMeasurement{{Stage: "check"}})
	if err := store.UpsertRun(ctx, withoutCost); err != nil {
		t.Fatalf("upsert run-without-cost: %v", err)
	}

	snapshot, err := store.HarvestCalibration(ctx, time.Time{}, time.Time{}, 1)
	if err != nil {
		t.Fatalf("harvest calibration: %v", err)
	}
	observations, ok := snapshot.Nodes["check"]
	if !ok {
		t.Fatal("missing node observations for check stage")
	}
	if len(observations.Costs) != 2 {
		t.Fatalf("cost observations = %#v, want two entries", observations.Costs)
	}
	var measured, unmeasured int
	for _, value := range observations.Costs {
		switch value {
		case 1:
			measured++
		case 0:
			unmeasured++
		default:
			t.Fatalf("unexpected empirical cost value %v", value)
		}
	}
	if measured != 1 || unmeasured != 1 {
		t.Fatalf("cost observations = %#v, want one measured and one unmeasured sample", observations.Costs)
	}
}

func TestHarvestCalibrationBoundaryTimestamps(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	whole := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	trailing := whole.Add(500 * time.Millisecond)
	for _, at := range []time.Time{whole, trailing} {
		p := projectionWithStages("run-"+at.Format(time.RFC3339Nano), "check")
		p.Run.StartedAt = at
		if err := store.UpsertRun(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name         string
		since, until time.Time
		want         int
	}{
		{"since whole", whole, time.Time{}, 2},
		{"since trailing", trailing, time.Time{}, 1},
		{"until whole", time.Time{}, whole, 1},
		{"until trailing", time.Time{}, trailing, 2},
		{"exact window", whole, trailing, 2},
	} {
		snapshot, err := store.HarvestCalibration(ctx, tc.since, tc.until, 1)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if snapshot.Runs != tc.want {
			t.Fatalf("%s: runs = %d, want %d", tc.name, snapshot.Runs, tc.want)
		}
	}
}
