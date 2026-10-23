package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestTemporalLadderAdvancesOneMinorToWorkerDeploymentGA(t *testing.T) {
	t.Parallel()
	var got []string
	for _, step := range temporalLadder {
		got = append(got, step.server+"/"+step.chart)
	}
	want := []string{
		"1.27.2/temporal.tgz",
		"1.27.4/temporal.tgz",
		"1.28.4/temporal-0.67.0.tgz",
		"1.29.7/temporal-0.73.2.tgz",
		"1.30.8/temporal-0.74.0.tgz",
		"1.31.4/temporal-1.6.0.tgz",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Temporal ladder = %v, want %v", got, want)
	}
}

func TestTemporalLadderValuesPinVersioningAndSchemaFirst(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"../../../../../deploy/reference/temporal/values.yaml",
		"../../../../../infra/temporal/values.yaml",
		"../../temporal-values-legacy.yaml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		text := string(data)
		for _, want := range []string{
			"frontend.workerVersioningWorkflowAPIs:",
			"system.enableDeploymentVersions:",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q", path, want)
			}
		}

		if strings.Contains(path, "values-legacy") {
			if !strings.Contains(text, "update:\n    enabled: true") {
				t.Errorf("%s does not enable the legacy schema update job", path)
			}
		} else if !strings.Contains(text, "manageSchema: true") ||
			!strings.Contains(text, "useHelmHooks: true") ||
			strings.Count(text, "connectProtocol: tcp") != 2 {
			t.Errorf("%s does not carry the complete chart 1.x schema and SQL connection contract", path)
		}
	}

}

func TestShardRangesAdvancedRequiresEveryShardToMove(t *testing.T) {
	t.Parallel()
	before := map[int64]int64{0: 17, 1: 22, 2: 9, 3: 41}
	for _, test := range []struct {
		name    string
		current map[int64]int64
		want    bool
	}{
		{"all advanced", map[int64]int64{0: 18, 1: 23, 2: 10, 3: 42}, true},
		{"pre-existing rows", map[int64]int64{0: 17, 1: 22, 2: 9, 3: 41}, false},
		{"one stale shard", map[int64]int64{0: 18, 1: 22, 2: 10, 3: 42}, false},
		{"missing shard", map[int64]int64{0: 18, 1: 23, 2: 10}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shardRangesAdvanced(before, test.current); got != test.want {
				t.Fatalf("shardRangesAdvanced() = %v, want %v", got, test.want)
			}
		})
	}
}
