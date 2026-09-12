package workflow

import (
	"testing"

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

// TestFeatureDefinitionsByDSLVersionEmptyResolvesNewestSupported pins the
// #3297 fallback: a gaggle/goober with zero workflows must probe at the newest
// LevelSupported DSL version. The pre-#3297 fallback returned an unpinned
// Definition{}, which the version router rewrote to V1DSLVersion ("1.4",
// deprecated) — a guaranteed validation failure the moment 1.4 turns
// unsupported, with no dslVersion field on GaggleSpec/GooberSpec for the
// author to act on.
func TestFeatureDefinitionsByDSLVersionEmptyResolvesNewestSupported(t *testing.T) {
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
		t.Fatalf("fallback DSL version = %q, want newest supported %q", got, supportmatrix.V2DSLVersion)
	}
}
