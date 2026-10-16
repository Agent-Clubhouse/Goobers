package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
)

// #6494: the API-only lane clears goobers:needs-remediation. A PR
// pr-comment-watch routed for human feedback that nothing has answered must
// reach the agentic path instead of having that label cleared, whether the
// branch is behind (API update) or already current (label-only clear).
func TestUpdateBehindPRRoutesUnansweredHumanCommentToFullRemediation(t *testing.T) {
	const ask = "Please add a short note to the README describing this change."
	human := map[string]interface{}{
		"id": 1, "user": map[string]string{"login": "alice"}, "body": ask, "created_at": "2026-10-01T12:00:00Z",
	}
	ownUnmarked := map[string]interface{}{
		"id": 1, "user": map[string]string{"login": "merge-review-bot"}, "body": ask, "created_at": "2026-10-01T12:00:00Z",
	}
	answer := map[string]interface{}{
		"id": 2, "user": map[string]string{"login": "merge-review-bot"}, "created_at": "2026-10-01T13:00:00Z",
		"body": stampOwnFixtureBody(withFeedbackAck("Added the note.", feedbackAckMarker(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))), "comment"),
	}
	tests := []struct {
		name         string
		current      bool
		identityMode string
		comments     []map[string]interface{}
		wantFull     bool
	}{
		{name: "behind with an unanswered human comment", comments: []map[string]interface{}{human}, wantFull: true},
		{name: "current with an unanswered human comment", current: true, comments: []map[string]interface{}{human}, wantFull: true},
		{name: "answered human comment", current: true, comments: []map[string]interface{}{human, answer}},
		{name: "dedicated: unmarked own-login comment is Goobers'", current: true, comments: []map[string]interface{}{ownUnmarked}},
		{name: "shared: unmarked own-login comment is a person's", current: true, identityMode: commentIdentityShared, comments: []map[string]interface{}{ownUnmarked}, wantFull: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mergeable := true
			state := &updateBehindServer{
				mergeable: &mergeable, current: test.current,
				labels: []string{needsRemediationLabel}, comments: test.comments,
			}
			if test.identityMode != "" {
				t.Setenv(executor.InputEnvVar("identityMode"), test.identityMode)
			}
			_, _, result := runUpdateBehindPRTest(t, state)
			if got := result["needsFullRemediation"] == "true"; got != test.wantFull {
				t.Fatalf("result = %v, want needsFullRemediation=%v", result, test.wantFull)
			}
			if kept := strings.Join(state.labels, ",") == needsRemediationLabel; kept != test.wantFull {
				t.Fatalf("labels = %v, want %s kept=%v", state.labels, needsRemediationLabel, test.wantFull)
			}
			if test.wantFull && state.updateCalls != 0 {
				t.Fatalf("update-branch calls = %d, want 0 when routing to full remediation", state.updateCalls)
			}
		})
	}
}
