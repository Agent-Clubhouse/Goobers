package main

import (
	"context"
	"io"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestUnparkSelfHealedEscalations is #992/#836: post-merge removes
// goobers:merge-escalated from a PR that has self-healed (its own head/base SHA
// moved past the escalation snapshot), while a genuine dead-end whose SHA is
// unchanged keeps the label — and a PR without the label is untouched. Before
// this, merge-escalated was never removed by any code path.
func TestUnparkSelfHealedEscalations(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	// #630: escalated, snapshot still matches current head/base -> stays parked.
	server.addIssue(630, "still-stuck pr")
	server.addOpenPR(630, "goobers/impl/stuck", "main", "h630", "b630", false, []string{remediationEscalatedLabel}, nil)
	stuck, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: "h630", EscalatedBaseSHA: "b630",
		EscalatedReason: "byte-identical diff, genuinely stuck",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(630, stuck)

	// #631: escalated, but its head SHA has moved since escalation -> un-escalated.
	server.addIssue(631, "self-healed pr")
	server.addOpenPR(631, "goobers/impl/healed", "main", "new-h631", "b631", false, []string{remediationEscalatedLabel}, nil)
	healed, err := remediationStateComment(remediationState{
		Escalated: true, EscalatedHeadSHA: "stale-h631", EscalatedBaseSHA: "b631",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(631, healed)

	// #632: not escalated at all -> untouched.
	server.addIssue(632, "unrelated pr")
	server.addOpenPR(632, "goobers/impl/other", "main", "h632", "b632", false, nil, nil)

	provider := server.newGitHubProvider("token")
	unparked, errs := unparkSelfHealedEscalations(context.Background(), provider, repo, 999, "main", io.Discard)
	if len(errs) != 0 {
		t.Fatalf("unparkSelfHealedEscalations errs = %v, want none", errs)
	}
	if len(unparked) != 1 || unparked[0] != 631 {
		t.Fatalf("unparked = %v, want exactly [631] (the self-healed PR)", unparked)
	}
}
