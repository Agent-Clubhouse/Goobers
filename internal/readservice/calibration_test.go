package readservice

import (
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
	v20 "github.com/goobers/goobers/internal/workflow/v_2_0"
)

func TestCalibrationFromSnapshot(t *testing.T) {
	windowStart := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	windowEnd := windowStart.Add(2 * time.Hour)
	snapshot := readmodel.CalibrationSnapshot{
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Runs:        12,
		MinSamples:  3,
		Outcomes:    map[string]int{"succeeded": 9, "failed": 3},
		Gates:       map[string]map[string]int{"approval": {"approve": 8, "reject": 2}},
		Nodes: map[string]readmodel.NodeCalibrationObservation{
			"implement": {
				Samples:    7,
				Successes:  5,
				Durations:  []time.Duration{time.Minute, 2 * time.Minute},
				RetryWaste: []float64{0.25, 0.5},
				Costs:      []float64{1.25, 2.5},
			},
		},
	}

	got := calibrationFromSnapshot(snapshot)
	want := v20.Calibration{
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Runs:        12,
		MinSamples:  3,
		Outcomes:    snapshot.Outcomes,
		Gates:       snapshot.Gates,
		Nodes: map[string]v20.NodeCalibration{
			"implement": {
				Samples:    7,
				Successes:  5,
				Durations:  []time.Duration{time.Minute, 2 * time.Minute},
				RetryWaste: []float64{0.25, 0.5},
				Costs:      []float64{1.25, 2.5},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calibrationFromSnapshot() = %#v, want %#v", got, want)
	}

	delete(snapshot.Nodes, "implement")
	if _, ok := got.Nodes["implement"]; !ok {
		t.Fatal("calibrationFromSnapshot() reused the snapshot node map")
	}
}
