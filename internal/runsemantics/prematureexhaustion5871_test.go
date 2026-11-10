package runsemantics

import (
	"testing"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/telemetry"
)

// The harness authors the rewrite; the runner decides its retry class and
// telemetry its error class. The spellings must never drift apart.
func TestPrematureExhaustionClaimCodeMatchesHarness(t *testing.T) {
	if PrematureExhaustionClaimCode != harness.ErrorCodePrematureExhaustionClaim {
		t.Fatalf("runner %q != harness %q", PrematureExhaustionClaimCode, harness.ErrorCodePrematureExhaustionClaim)
	}
	if class := telemetry.ClassifyError(PrematureExhaustionClaimCode); class != telemetry.ErrorClassValidation {
		t.Fatalf("%s classifies as %q, want %q (an unbacked claim, not a timeout)", PrematureExhaustionClaimCode, class, telemetry.ErrorClassValidation)
	}
}
