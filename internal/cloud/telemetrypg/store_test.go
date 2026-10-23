package telemetrypg

import (
	"testing"

	"github.com/goobers/goobers/internal/telemetryclient"
)

func finding(kind string, metrics map[string]float64, runs ...string) telemetryclient.Finding {
	f := telemetryclient.Finding{Kind: kind, Subject: "s", Metrics: metrics}
	for _, id := range runs {
		f.FlaggedRuns = append(f.FlaggedRuns, telemetryclient.JournalPointer{RunID: id})
	}
	return f
}

func TestMergeContributionsCombinesLikeTheLocalRollup(t *testing.T) {
	for _, test := range []struct {
		name string
		in   []telemetryclient.Finding
		want map[string]float64
	}{
		{"stage failure rate", []telemetryclient.Finding{
			finding("stage-failure-rate", map[string]float64{"failureRate": 0.9, "totalAttempts": 10, "failedAttempts": 9}),
			finding("stage-failure-rate", map[string]float64{"failureRate": 0.5, "totalAttempts": 10, "failedAttempts": 5}),
		}, map[string]float64{"failureRate": 0.7, "totalAttempts": 20, "failedAttempts": 14}},
		{"error signature", []telemetryclient.Finding{
			finding("error-signature", map[string]float64{"count": 3}),
			finding("error-signature", map[string]float64{"count": 4}),
		}, map[string]float64{"count": 7}},
		{"gate repass churn", []telemetryclient.Finding{
			finding("gate-repass-churn", map[string]float64{"escalationRate": 1, "totalEvaluations": 2, "escalatedCount": 2}),
			finding("gate-repass-churn", map[string]float64{"escalationRate": 0, "totalEvaluations": 8, "escalatedCount": 0}),
		}, map[string]float64{"escalationRate": 0.2, "totalEvaluations": 10, "escalatedCount": 2}},
		{"gate never fails", []telemetryclient.Finding{
			finding("gate-never-fails", map[string]float64{"totalEvaluations": 6}),
			finding("gate-never-fails", map[string]float64{"totalEvaluations": 5}),
		}, map[string]float64{"totalEvaluations": 11}},
		{"credit assignment: newest stands", []telemetryclient.Finding{
			finding("credit-assignment", map[string]float64{"runs": 9, "failureShare": 0.8}),
			finding("credit-assignment", map[string]float64{"runs": 50, "failureShare": 0.1}),
		}, map[string]float64{"runs": 9, "failureShare": 0.8}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := mergeContributions(test.in, 10).Metrics
			if len(got) != len(test.want) {
				t.Fatalf("metrics = %v, want %v", got, test.want)
			}
			for key, want := range test.want {
				if got[key] != want {
					t.Fatalf("metrics = %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestMergeContributionsFlaggedRunsDeduplicateAndBound(t *testing.T) {
	merged := mergeContributions([]telemetryclient.Finding{
		finding("error-signature", map[string]float64{"count": 1}, "c", "b"),
		finding("error-signature", map[string]float64{"count": 1}, "b", "a"),
	}, 3)
	var ids []string
	for _, pointer := range merged.FlaggedRuns {
		ids = append(ids, pointer.RunID)
	}
	if len(ids) != 3 || ids[0] != "c" || ids[1] != "b" || ids[2] != "a" {
		t.Fatalf("flagged runs = %v, want newest contribution first, de-duplicated", ids)
	}
	if bounded := mergeContributions([]telemetryclient.Finding{finding("error-signature", map[string]float64{"count": 1}, "x", "y", "z")}, 2); len(bounded.FlaggedRuns) != 2 {
		t.Fatalf("flagged runs not bounded: %v", bounded.FlaggedRuns)
	}
}
