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
