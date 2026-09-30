package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// openPRIssueNumbers only needs PR bodies; it must not resolve per-PR check
// state, which cost two API calls per open PR on every backlog query.
func TestOpenPRIssueNumbersSkipsPerPRCheckState(t *testing.T) {
	server := newFakeGitHubServer(t, "acme", "app")
	for i, n := range []int{101, 102, 103} {
		server.addOpenPR(n, "goobers/implementation/run-"+string(rune('a'+i)), "main", "head", "base", false, nil, nil)
	}
	server.setPRBody(101, "Fixes #7")
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}

	got, err := openPRIssueNumbers(context.Background(), server.newGitHubProvider("pr-token"), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !got["7"] {
		t.Fatalf("open PR issue numbers = %v, want #7 from PR #101's body", got)
	}
	server.mu.Lock()
	checks := server.checkStateRequests
	server.mu.Unlock()
	if checks != 0 {
		t.Fatalf("check-state requests = %d, want 0: only PR bodies are needed", checks)
	}
}

// An in-review issue that an open PR still references cannot be
// closed-unmerged, so reconciliation must not read its comments or linked PRs.
// The breadcrumb here names a PR the server does not have: reading it would
// fail, so success proves the issue was skipped.
func TestReconcileClosedUnmergedInReviewSkipsIssuesWithOpenPRs(t *testing.T) {
	server := newFakeGitHubServer(t, "acme", "app")
	server.addIssue(7, "Implement safely", "goobers:approved", "goobers:ready", inReviewStatusLabel)
	server.addComment(7, implementationInReviewComment("https://github.com/acme/app/pull/999"))
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}

	err := reconcileClosedUnmergedInReview(context.Background(),
		server.newGitHubProvider("issues-token"), server.newGitHubProvider("pr-token"), repo,
		map[string]bool{"7": true})
	if err != nil {
		t.Fatalf("reconcile read the linked PR of an issue an open PR references: %v", err)
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAllLabels(labels, []string{inReviewStatusLabel}) {
		t.Fatalf("labels = %v, want %q kept for an issue with an open PR", labels, inReviewStatusLabel)
	}
}
