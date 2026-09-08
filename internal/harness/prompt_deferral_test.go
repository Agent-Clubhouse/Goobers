package harness

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestReviewerDeferralPromptRequiresRunnerCapability(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		req := RunRequest{Mode: ModeReview, Envelope: apiv1.InvocationEnvelope{ReviewerDeferralAllowed: allowed}}
		for name, render := range map[string]func(RunRequest) string{
			"file": renderPrompt, "response": renderResponseCompletionPrompt,
			"file recovery": renderCompletionRecoveryPrompt, "response recovery": renderResponseCompletionRecoveryPrompt,
		} {
			prompt := render(req)
			if got := strings.Contains(prompt, `"needs-changes"|"defer"`); got != allowed {
				t.Fatalf("%s: advertised deferral=%v, runner capability=%v", name, got, allowed)
			}
			if allowed && (!strings.Contains(prompt, `reasonCode "ordering" or "no-lander"`) || !strings.Contains(prompt, "never grants landing authority")) {
				t.Fatalf("%s: incomplete deferral contract: %s", name, prompt)
			}
		}
	}
}
