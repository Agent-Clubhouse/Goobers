package readservice

import (
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// TestProjectedMeasurementMatchesTheOldPerCandidateCheck is the differential
// test for #1782.
//
// The population predicates moved from being evaluated per candidate inside an
// HTTP request (matchesTelemetryAttempts) to being projected once per run and
// read from an index. That is a performance change, and a performance change
// that alters results is a bug, not an optimisation.
//
// So this compares the two implementations directly over an attempt corpus that
// exercises every branch: measured and unmeasured, known and unknown branches,
// repeated traversals, and the same stage appearing at more than one branch.
func TestProjectedMeasurementMatchesTheOldPerCandidateCheck(t *testing.T) {
	corpora := map[string][]rollup.StageAttempt{
		"empty": {},
		"one unmeasured attempt": {
			{Stage: "build", Traversal: 1, BranchKnown: true},
		},
		"tokens need both halves": {
			// InputTokens alone must NOT count: the old check required both.
			{Stage: "build", Traversal: 1, BranchKnown: true, InputTokens: int64Ptr(10)},
		},
		"tokens complete": {
			{Stage: "build", Traversal: 1, BranchKnown: true,
				InputTokens: int64Ptr(10), OutputTokens: int64Ptr(20)},
		},
		"cost and premium on different stages": {
			{Stage: "build", Traversal: 1, BranchKnown: true, CostUSD: floatPtr(1.5)},
			{Stage: "test", Traversal: 1, BranchKnown: true, CopilotPremiumRequests: floatPtr(2)},
		},
		"retry waste within one branch": {
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 1},
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 2},
		},
		"no retry waste across different branches": {
			// Different branches, so neither attempt was superseded.
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 1},
			{Stage: "build", Branch: 1, BranchKnown: true, Traversal: 2},
		},
		"branch-known is part of the key": {
			// Same numeric branch, but one is unknown — a distinct key, so no
			// waste. Dropping BranchKnown from the key would flag both.
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 1},
			{Stage: "build", Branch: 0, BranchKnown: false, Traversal: 2},
		},
		"waste in one stage does not implicate another": {
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 1},
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 2},
			{Stage: "test", Branch: 0, BranchKnown: true, Traversal: 1},
		},
		"everything at once": {
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 1,
				InputTokens: int64Ptr(1), OutputTokens: int64Ptr(2), CostUSD: floatPtr(3)},
			{Stage: "build", Branch: 0, BranchKnown: true, Traversal: 2,
				CopilotPremiumRequests: floatPtr(4)},
			{Stage: "deploy", Branch: 2, BranchKnown: true, Traversal: 7},
		},
	}

	populations := []StagePopulation{
		StagePopulationTokenMeasured,
		StagePopulationPremiumMeasured,
		StagePopulationCostMeasured,
		StagePopulationRetryWaste,
	}
	stages := []string{"", "build", "test", "deploy", "absent"}

	for name, attempts := range corpora {
		projected := measurementFromAttempts(attempts)
		byStage := make(map[string]int, len(projected))
		for i, m := range projected {
			byStage[m.Stage] = i
		}

		for _, population := range populations {
			for _, stage := range stages {
				t.Run(fmt.Sprintf("%s/%s/%s", name, population, stageLabel(stage)), func(t *testing.T) {
					want := matchesTelemetryAttempts(attempts, stage, population)
					got := projectedMatches(projected, byStage, stage, population)
					if got != want {
						t.Errorf("stage=%q population=%s: projected %v, per-candidate check %v\n"+
							"the answer changed when the evaluation moved to the write path",
							stage, population, got, want)
					}
				})
			}
		}
	}
}

// projectedMatches answers the filter the way the SQL does: the stage-scoped
// form reads one stage's flag, the unscoped form is the OR across stages.
func projectedMatches(projected []readmodel.StageMeasurement, byStage map[string]int,
	stage string, population StagePopulation) bool {
	if stage != "" {
		i, ok := byStage[stage]
		if !ok {
			return false
		}
		return flagFor(projected[i], population)
	}
	for _, m := range projected {
		if flagFor(m, population) {
			return true
		}
	}
	return false
}

func flagFor(m readmodel.StageMeasurement, population StagePopulation) bool {
	switch population {
	case StagePopulationTokenMeasured:
		return m.TokenMeasured
	case StagePopulationPremiumMeasured:
		return m.PremiumMeasured
	case StagePopulationCostMeasured:
		return m.CostMeasured
	case StagePopulationRetryWaste:
		return m.RetryWaste
	default:
		return false
	}
}

func stageLabel(stage string) string {
	if stage == "" {
		return "any-stage"
	}
	return stage
}

func int64Ptr(v int64) *int64     { return &v }
func floatPtr(v float64) *float64 { return &v }
