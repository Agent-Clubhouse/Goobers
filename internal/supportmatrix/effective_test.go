package supportmatrix

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEffectiveInRecordsActualRemovalWithoutRewritingPromise(t *testing.T) {
	matrix := GetDSL()
	legacy := matrix[V1DSLVersion]
	if legacy.EffectiveIn != "v0.4.0" || legacy.Level != LevelUnsupported {
		t.Fatalf("legacy support = %+v, want actual v0.4.0 removal", legacy)
	}
	if !reflect.DeepEqual(legacy.History, betaTwoSupportMatrix()[V1DSLVersion].History) {
		t.Fatal("the correction must preserve the history already published in beta.2")
	}
	for _, release := range []string{"v0.4.0", "v0.5.0"} {
		if err := ValidateSupportPolicyForRelease(matrix, release); err != nil {
			t.Fatalf("corrected support policy for %s: %v", release, err)
		}
	}
	if err := ValidateSupportPolicyForRelease(matrix, "v0.3.3"); err == nil {
		t.Fatal("effectiveIn must not make the future removal valid before v0.4.0")
	}
	if err := validateSupportMatrixEvolution(lastReleasedSupportMatrix(), matrix, "v0.3.3",
		map[string]releaseVersion{"1.4": {minor: 1}, "2.0": {minor: 1}}); err != nil {
		t.Fatalf("policy history and published support windows remain valid: %v", err)
	}

	encoded, err := json.Marshal(matrix.Versions())
	if err != nil {
		t.Fatal(err)
	}
	var rows []Version
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatal(err)
	}
	if got := versionsToSupportMatrix(rows); !reflect.DeepEqual(got, matrix) {
		t.Fatalf("support snapshot roundtrip lost the correction: %s", encoded)
	}
	if strings.Count(string(encoded), `"effectiveIn"`) != 1 {
		t.Fatalf("only the corrected row should emit effectiveIn: %s", encoded)
	}
}

func TestEffectiveInCannotBypassOtherSupportTransitions(t *testing.T) {
	for _, tc := range []struct{ name, version, effective string }{
		{"other DSL", "2.0", "v0.4.0"},
		{"arbitrary earlier removal", "1.4", "v0.3.0"},
		{"arbitrary later removal", "1.4", "v0.6.0"},
		{"prerelease date", "1.4", "v0.4.0-beta.1"},
		{"invalid date", "1.4", "tomorrow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matrix := GetDSL()
			row := matrix[tc.version]
			row.EffectiveIn = tc.effective
			matrix[tc.version] = row
			if err := ValidateSupportPolicy(matrix); err == nil {
				t.Fatal("arbitrary effectiveIn bypassed the normal transition policy")
			}
		})
	}
}

func TestPublishedEffectiveInCannotBeRemoved(t *testing.T) {
	released, current := GetDSL(), GetDSL()
	row := current[V1DSLVersion]
	row.EffectiveIn = ""
	current[V1DSLVersion] = row
	err := validateSupportMatrixEvolution(released, current, "v0.4.0",
		map[string]releaseVersion{"1.4": {minor: 1}, "2.0": {minor: 1}})
	if err == nil || !strings.Contains(err.Error(), "effectiveIn correction must not change") {
		t.Fatalf("evolution = %v, want immutable correction", err)
	}
}
