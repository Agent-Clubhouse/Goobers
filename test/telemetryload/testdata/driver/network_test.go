package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

func TestNetworkFaultPhaseBoundaries(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		mode    int32
	}{{0, 1}, {5*time.Minute - 1, 1}, {5 * time.Minute, 2}, {10 * time.Minute, 3}, {15 * time.Minute, 4}, {20 * time.Minute, 5}, {25 * time.Minute, 0}, {30 * time.Minute, 0}} {
		if got := networkFaultMode(tc.elapsed, 30*time.Minute); got != tc.mode {
			t.Errorf("elapsed=%s got=%d want=%d", tc.elapsed, got, tc.mode)
		}
	}
}

func TestNetworkValidationRequiresExercisedFaults(t *testing.T) {
	valid := func() Result {
		counts := make(map[string]int64)
		for _, name := range networkModeNames {
			counts[name] = 1
		}
		return Result{Name: "network-faults", Runs: 1, ExpectedRunEvents: 1,
			Replay: telemetry.AzureReplayStats{AccountingReady: true}, NetworkModes: counts, Duplicates: 1}
	}
	validate(valid())
	for _, name := range append(networkModeNames[:], "no-duplicate") {
		t.Run(name, func(t *testing.T) {
			result := valid()
			if name == "no-duplicate" {
				result.Duplicates = 0
			} else {
				result.NetworkModes[name] = 0
			}
			defer func() {
				if recover() == nil {
					t.Fatal("unexercised network fault passed validation")
				}
			}()
			validate(result)
		})
	}
}
