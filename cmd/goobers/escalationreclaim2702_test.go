package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// escalation2702Fixture stands up a PR parked on sibling overlap: the
// escalation snapshot pins head/base, the blocked-on-sibling payload names the
// blocker, and the live base has since advanced past the pinned tip.
func escalation2702Fixture(t *testing.T, blocker int, blockerOpen bool) (remediationProvider, providers.RepositoryRef, providers.PullRequestSummary) {
	t.Helper()
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(100, "parked pr")
	server.addIssue(blocker, "blocking sibling")
	if !blockerOpen {
		server.closeIssue(blocker)
	}
	parkComment, err := remediationStateComment(remediationState{
		Escalated:            true,
		EscalationGeneration: 1,
		EscalationCauses:     []remediationCause{remediationCauseSiblingOverlap},
		EscalatedHeadSHA:     "head100",
		EscalatedBaseSHA:     "base-at-escalation",
	})
	if err != nil {
		t.Fatalf("remediationStateComment: %v", err)
	}
	server.addComment(100, parkComment)
	server.addComment(100, blockedOnSiblingCommentFor(t, blocker))
	// The base has moved on since the park — the shape that used to re-open
	// eligibility every tick on an active repository.
	server.setBranchTip("main", "base-advanced")

	pr := providers.PullRequestSummary{
		Number: 100, State: "open", Base: "main",
		Head: "goobers/implementation/run-100", HeadSHA: "head100",
		Labels: []string{remediationEscalatedLabel},
	}
	return server.newGitHubProvider("token"), repo, pr
}

// TestEscalatedPRIsNotReclaimedWhenOnlyTheBaseMoved is #2702's regression pin.
//
// A PR escalated behind a sibling awaiting a human ordering decision was
// re-claimed on every scheduling tick whose base had advanced — daily for five
// consecutive days on a live instance — running the full remediation pipeline
// each time to reach the same terminal answer.
//
// The park's cause is sibling overlap, which baseAdvanceCuresRemediationCause
// calls rebase-curable, and correctly so: the canonical cure IS the sibling
// landing, which advances the base. But an active repository advances its base
// constantly for unrelated reasons, and each advance was read as news.
func TestEscalatedPRIsNotReclaimedWhenOnlyTheBaseMoved(t *testing.T) {
	provider, repo, pr := escalation2702Fixture(t, 99, true)

	blocks, err := escalationStillBlocks(context.Background(), provider, repo, pr)
	if err != nil {
		t.Fatalf("escalationStillBlocks: %v", err)
	}
	if !blocks {
		t.Fatal("blocks = false, want true: blocker #99 is still open, so the base advance " +
			"says nothing about this park and must not spend a remediation cycle")
	}
}

// TestEscalatedPRIsReclaimedWhenTheBlockerResolves is the other half, and the
// reason this narrowing is safe: when the ordering situation actually changes,
// the park releases on the very next base advance. Without this the fix would
// rebuild the permanent park #4038 and #4051 each had to undo, where
// pr-remediation excludes escalated PRs upstream so the head can never move
// and the advertised exit is unreachable.
func TestEscalatedPRIsReclaimedWhenTheBlockerResolves(t *testing.T) {
	provider, repo, pr := escalation2702Fixture(t, 99, false)

	blocks, err := escalationStillBlocks(context.Background(), provider, repo, pr)
	if err != nil {
		t.Fatalf("escalationStillBlocks: %v", err)
	}
	if blocks {
		t.Fatal("blocks = true, want false: blocker #99 has resolved, which is exactly the " +
			"state change worth a remediation cycle")
	}
}

// TestEscalationCausesAreOrderingOnly pins the narrowing's boundary. Only a
// park whose sole rebase-curable cause is sibling overlap may hold through a
// base advance; a conflict or failing-CI park must keep unparking, because a
// base advance genuinely re-decides both (#4058).
func TestEscalationCausesAreOrderingOnly(t *testing.T) {
	tests := []struct {
		name   string
		causes []remediationCause
		want   bool
	}{
		{name: "sibling overlap alone", causes: []remediationCause{remediationCauseSiblingOverlap}, want: true},
		{name: "no causes recorded", causes: nil, want: false},
		{
			name:   "sibling overlap with a conflict",
			causes: []remediationCause{remediationCauseSiblingOverlap, remediationCauseConflict},
			want:   false,
		},
		{
			name:   "sibling overlap with failing CI",
			causes: []remediationCause{remediationCauseSiblingOverlap, remediationCauseFailingCI},
			want:   false,
		},
		{name: "conflict alone", causes: []remediationCause{remediationCauseConflict}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escalationCausesAreOrderingOnly(remediationState{
				Escalated: true, EscalationGeneration: 1, EscalationCauses: tt.causes,
			})
			if got != tt.want {
				t.Fatalf("escalationCausesAreOrderingOnly(%v) = %v, want %v", tt.causes, got, tt.want)
			}
		})
	}
}
