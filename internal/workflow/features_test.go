package workflow

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestFeatureDefinitionsByDSLVersionFansOutPerPin(t *testing.T) {
	definitions := FeatureDefinitionsByDSLVersion([]Definition{
		{Name: "next", DSLVersion: supportmatrix.V2DSLVersion},
		{Name: "legacy", DSLVersion: supportmatrix.V1DSLVersion},
		// A duplicate pin collapses: the fan-out is per distinct version, not
		// per workflow.
		{Name: "next-sibling", DSLVersion: supportmatrix.V2DSLVersion},
	})
	if len(definitions) != 2 {
		t.Fatalf("definitions = %+v, want one probe per distinct pin", definitions)
	}
	if definitions[0].DSLVersion != supportmatrix.V1DSLVersion ||
		definitions[1].DSLVersion != supportmatrix.V2DSLVersion {
		t.Fatalf("definitions = %+v, want ascending version order [%q %q]",
			definitions, supportmatrix.V1DSLVersion, supportmatrix.V2DSLVersion)
	}
}

func TestFeatureDefinitionsByDSLVersionEmptyResolvesOldestSupported(t *testing.T) {
	definitions := FeatureDefinitionsByDSLVersion(nil)
	if len(definitions) != 1 {
		t.Fatalf("definitions = %+v, want exactly one fallback probe", definitions)
	}
	got := definitions[0].DSLVersion
	if got == "" || got == supportmatrix.V1DSLVersion {
		t.Fatalf("fallback DSL version = %q; must not be unpinned or the deprecated transitional default %q",
			got, supportmatrix.V1DSLVersion)
	}
	support, ok := supportmatrix.GetDSL().Lookup(got)
	if !ok || support.Level != supportmatrix.LevelSupported {
		t.Fatalf("fallback DSL version %q level = %q, %v, want %q", got, support.Level, ok, supportmatrix.LevelSupported)
	}
	if got != supportmatrix.V2DSLVersion {
		t.Fatalf("fallback DSL version = %q, want oldest supported %q", got, supportmatrix.V2DSLVersion)
	}
}

// Exercise the proposed graduation without mutating the compiled matrix.
func TestWorkflowlessCompatibilityAcrossGraduation(t *testing.T) {
	matrix := supportmatrix.GetDSL()
	entry := matrix[supportmatrix.V3DSLVersion]
	entry.Level = supportmatrix.LevelSupported
	matrix[supportmatrix.V3DSLVersion] = entry
	probes := featureDefinitionsByDSLVersion(nil, matrix)
	if len(probes) != 1 || probes[0].DSLVersion != "2.0" {
		t.Fatalf("graduated fallback = %+v, want 2.0", probes)
	}
	for _, spec := range []apiv1.GaggleSpec{
		{},
		{RequiredCapabilities: []string{"dotnet@8"}},
		{RequiredCapabilities: []string{"os=windows"}},
	} {
		if _, err := FeaturesForGaggle(probes[0], spec); err != nil {
			t.Fatalf("legacy workflow-less gaggle %+v: %v", spec, err)
		}
	}
	if _, err := FeaturesForGoober(probes[0], apiv1.GooberSpec{}); err != nil {
		t.Fatalf("workflow-less goober: %v", err)
	}
	if newest, _ := matrix.NewestSupported(); newest != "3.0" {
		t.Fatalf("new authoring must still resolve 3.0, got %q", newest)
	}
	for _, version := range []string{"2.0", "2.1", "3.0", "3.1"} {
		got := featureDefinitionsByDSLVersion([]Definition{{DSLVersion: version}}, matrix)
		if got[0].DSLVersion != version {
			t.Fatalf("explicit %s pin changed to %s", version, got[0].DSLVersion)
		}
	}
	if _, err := FeaturesForGaggle(Definition{DSLVersion: "3.0"}, apiv1.GaggleSpec{RequiredCapabilities: []string{"dotnet@8"}}); err == nil {
		t.Fatal("explicit 3.0 pin must still reject requiredCapabilities")
	}
	if _, err := StagePlacements(probes[0], apiv1.GaggleSpec{RunsOn: &apiv1.GaggleRunsOn{OS: "linux"}}, nil); err == nil {
		t.Fatal("2.0 placement must not silently accept 3.0 runsOn")
	}
	for _, level := range []supportmatrix.Level{supportmatrix.LevelDeprecated, supportmatrix.LevelUnsupported} {
		entry := matrix["2.0"]
		entry.Level = level
		matrix["2.0"] = entry
		got := featureDefinitionsByDSLVersion(nil, matrix)
		if got[0].DSLVersion != "3.0" {
			t.Fatalf("2.0 %s: fallback = %+v, want 3.0", level, got)
		}
	}
}
