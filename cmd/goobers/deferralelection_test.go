package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestNoLanderDeferralExclusionRequiresTrustedCurrentPins(t *testing.T) {
	for _, tc := range []struct {
		name, author, head, base string
		reason                   apiv1.VerdictReasonCode
		want                     bool
	}{
		{"current no lander", "goobers", "head", "base", apiv1.VerdictReasonNoLander, true},
		{"head advanced", "goobers", "old", "base", apiv1.VerdictReasonNoLander, false},
		{"base advanced", "goobers", "head", "old", apiv1.VerdictReasonNoLander, false},
		{"missing pin", "goobers", "", "base", apiv1.VerdictReasonNoLander, false},
		{"spoofed", "mallory", "head", "base", apiv1.VerdictReasonNoLander, false},
		{"ordinary ordering", "goobers", "head", "base", apiv1.VerdictReasonOrdering, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeGitHubServer(t, "acme", "app")
			server.addIssue(10, "candidate")
			server.addCommentAs(10, tc.author, renderVerdictComment(apiv1.Verdict{
				Decision: apiv1.VerdictDefer, ReasonCode: tc.reason, Rationale: "Deferred election.", HeadSHA: tc.head, BaseSHA: tc.base,
			}))
			set, err := electionIneligibleSet(context.Background(), server.newGitHubProvider("token"), providers.RepositoryRef{Owner: "acme", Name: "app"}, []providers.PullRequestSummary{
				{Number: 10, HeadSHA: "head", BaseSHA: "base", Labels: []string{blockedOnSiblingLabel}},
			})
			if err != nil || set[10] != tc.want {
				t.Fatalf("election exclusion=%v want=%v err=%v", set, tc.want, err)
			}
		})
	}
}
