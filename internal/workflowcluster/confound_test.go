package workflowcluster

import (
	"reflect"
	"slices"
	"testing"
)

func twoBlobModel(t *testing.T) Model {
	t.Helper()
	model, err := Fit("test/v1", slices.Concat(blob("a", 0, 0, 9), blob("b", 10, 0, 9)),
		Options{MinClusterSize: 4, Resamples: 10, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2", len(model.Clusters))
	}
	return model
}

func TestConfoundCheckFlagsClustersDrivenByModelSwap(t *testing.T) {
	model := twoBlobModel(t)
	strata := Strata{}
	for i, c := range model.Clusters {
		for _, id := range c.Members {
			strata[id] = []string{"model-old", "model-new"}[i]
		}
	}

	report := ConfoundCheck(model, strata, ConfoundOptions{Seed: 1})
	if !report.Confounded || report.Association < 0.99 || report.PValue > 0.01 {
		t.Fatalf("report = %+v, want confounded", report)
	}
	if report.Evidence != EvidenceObservational || report.Tested != 18 || len(report.Undersized) != 0 {
		t.Fatalf("report = %+v", report)
	}
	gym := ConfoundCheck(model, strata, ConfoundOptions{Seed: 1, Evidence: EvidenceGymControlled})
	if gym.Evidence != EvidenceGymControlled {
		t.Fatalf("evidence = %q", gym.Evidence)
	}
}

func TestConfoundCheckBalancedStrataAreNotConfounded(t *testing.T) {
	model := twoBlobModel(t)
	strata := Strata{}
	for _, c := range model.Clusters {
		for i, id := range c.Members {
			strata[id] = []string{"harness-1", "harness-2", "harness-3"}[i%3]
		}
	}

	report := ConfoundCheck(model, strata, ConfoundOptions{Seed: 1})
	if report.Confounded || report.Association > 0.01 || report.PValue < 0.5 {
		t.Fatalf("report = %+v, want not confounded", report)
	}
}

func TestConfoundCheckReportsUndersizedStrataWithoutPooling(t *testing.T) {
	model := twoBlobModel(t)
	strata := Strata{}
	for i, c := range model.Clusters {
		for _, id := range c.Members {
			strata[id] = []string{"cohort-a", "cohort-b"}[i]
		}
	}
	rare := []string{model.Clusters[0].Members[0], model.Clusters[1].Members[0]}
	strata[rare[0]] = "cohort-rare-x"
	strata[rare[1]] = "cohort-rare-y"
	delete(strata, model.Clusters[1].Members[1])

	report := ConfoundCheck(model, strata, ConfoundOptions{MinStratumSize: 3, Seed: 1})
	if !slices.Equal(report.Undersized, []string{"cohort-rare-x", "cohort-rare-y"}) {
		t.Fatalf("undersized = %v", report.Undersized)
	}
	if len(report.Strata) != 4 || report.Tested != 18-3 || report.Unlabeled != 1 {
		t.Fatalf("report = %+v", report)
	}
	for _, s := range report.Strata {
		if s.Key == "cohort-rare-x" && s.Size != 1 {
			t.Fatalf("stratum %+v", s)
		}
	}
}

func TestConfoundCheckIsDeterministic(t *testing.T) {
	model := twoBlobModel(t)
	strata := Strata{}
	for _, c := range model.Clusters {
		for i, id := range c.Members {
			strata[id] = []string{"x", "y"}[min(i%4, 1)]
		}
	}
	first := ConfoundCheck(model, strata, ConfoundOptions{Seed: 9, Permutations: 200})
	second := ConfoundCheck(model, strata, ConfoundOptions{Seed: 9, Permutations: 200})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reports differ:\n%+v\n%+v", first, second)
	}
}
