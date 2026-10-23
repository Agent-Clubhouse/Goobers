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
		"1.31.0/temporal-1.3.0.tgz",
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
