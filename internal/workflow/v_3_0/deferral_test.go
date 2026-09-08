package v30

import "testing"

func TestCompileExplicitReviewerDeferralBranch(t *testing.T) {
	spec := gatedSpec()
	spec.Gates[0].Branches["defer"] = TerminalComplete
	if _, err := compileAcknowledged(Definition{Name: "review-deferral", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("explicit deferral route refused: %v", err)
	}
}
