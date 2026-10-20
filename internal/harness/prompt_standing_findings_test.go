package harness

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// #5890: every reviewer completion contract says how to answer standing
// findings, with or without the deferral vocabulary, and a task completion
// contract never does.
func TestReviewerPromptExplainsStandingFindingDispositions(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		req := RunRequest{Mode: ModeReview, Envelope: apiv1.InvocationEnvelope{ReviewerDeferralAllowed: allowed}}
		for name, render := range map[string]func(RunRequest) string{
			"file": renderPrompt, "response": renderResponseCompletionPrompt,
			"file recovery": func(req RunRequest) string { return renderCompletionRepairPrompt(req, nil) },
		} {
			prompt := render(req)
			for _, want := range []string{"standingFindings", `"dispositions"`, `"outcome": "resolved", "evidence"`, `"outcome": "rejected", "reason"`, "published as needs-changes"} {
				if !strings.Contains(prompt, want) {
					t.Fatalf("%s (deferral=%v): prompt lacks %q", name, allowed, want)
				}
			}
		}
	}
	if prompt := renderPrompt(RunRequest{}); strings.Contains(prompt, "standingFindings") {
		t.Fatal("a task completion contract mentions standing findings")
	}
}
