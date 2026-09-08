package prqueue

import (
	"strings"
	"testing"
)

func TestNextStepsCoverExistingExclusionVocabulary(t *testing.T) {
	for _, reason := range []string{Draft, Checks, Label, AdvisoryPublished, Policy, ScopeGate, Escalated, Demoted, SiblingBlocked, TutorSignoff} {
		step := NextStep(reason)
		if step == "" || step == NextStep("future-reason") {
			t.Errorf("reason %q has no specific next step", reason)
		}
	}
	if !strings.Contains(NextStep("future-reason"), "no automatic recovery action") {
		t.Fatal("unknown reason invented a recovery action")
	}
}
