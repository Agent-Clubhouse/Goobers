package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestPinnedDeferralCannotAuthorizeMerge(t *testing.T) {
	for _, reason := range []apiv1.VerdictReasonCode{apiv1.VerdictReasonOrdering, apiv1.VerdictReasonNoLander} {
		t.Run(string(reason), func(t *testing.T) {
			verdict := apiv1.Verdict{Decision: apiv1.VerdictDefer, ReasonCode: reason, Rationale: "Wait for the sibling.", HeadSHA: "head123", BaseSHA: "base456"}
			poll := providers.PullRequestPollResult{
				Title: "Deferred candidate", HeadSHA: verdict.HeadSHA, BaseSHA: verdict.BaseSHA,
				CommentsSince: []providers.PullRequestComment{{Author: "goobers", Body: renderVerdictComment(verdict)}},
			}
			if _, ok := pinnedPassVerdict(poll, "goobers"); ok {
				t.Fatal("trusted SHA-pinned deferral authorized a merge")
			}
			if _, _, err := structuredMergeCommitMessage(poll, "goobers"); err == nil {
				t.Fatal("deferral generated an authorized merge commit message")
			}
		})
	}
}
