package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

// #4154: goobers:needs-remediation is terminal on an ISSUE — nothing anywhere
// removes it — and local-gate's `infra` branch parks items with it, charging
// the item permanently for a failure failure-class had just attributed to the
// substrate. Twenty goobers:cloud items were in that state, which is why
// backlog-curation reported no work against a backlog that was not drained.
func TestStaleInfrastructureRemediationPark(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("no label, nothing to clear", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "issue 1")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "1"}

		stale, err := staleInfrastructureRemediationPark(context.Background(), provider, repo, item)
		if err != nil {
			t.Fatalf("staleInfrastructureRemediationPark: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — the issue carries no park")
		}
	})

	// The live case, verbatim: #3179 and #3180 both carried exactly this.
	t.Run("an infrastructure park clears", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(2, "issue 2", needsRemediationLabel)
		server.addComment(2, "goobers-claim: run=f071bf34b79a5fe06a5452a8c0165ac2")
		server.addComment(2, remediationParkCommentPrefix+"gate local-gate returned terminal outcome infra")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "2", Labels: []string{needsRemediationLabel}}

		stale, err := staleInfrastructureRemediationPark(context.Background(), provider, repo, item)
		if err != nil {
			t.Fatalf("staleInfrastructureRemediationPark: %v", err)
		}
		if !stale {
			t.Fatal("stale = false, want true — the park names an infrastructure outcome")
		}
	})

	// THE POLARITY TEST. Removing a label is an action, so a merit park — the
	// implementer genuinely failing to produce a working change — must survive
	// untouched. Clearing this one would re-offer an item that has already
	// proven it needs a human's attention.
	t.Run("a merit park is left alone", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(3, "issue 3", needsRemediationLabel)
		server.addComment(3, remediationParkCommentPrefix+
			`the prior repass was triggered by gate "review" outcome "needs-changes" after stage `+
			`"implement" failed with an implementation failure (code "failure"): Incomplete `+
			`implementation; the implementer produced no change in response`)
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "3", Labels: []string{needsRemediationLabel}}

		stale, err := staleInfrastructureRemediationPark(context.Background(), provider, repo, item)
		if err != nil {
			t.Fatalf("staleInfrastructureRemediationPark: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — a merit park is not the substrate's fault")
		}
	})

	t.Run("no park comment at all fails closed", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(4, "issue 4", needsRemediationLabel)
		server.addComment(4, "someone applied the label by hand and said why in prose")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "4", Labels: []string{needsRemediationLabel}}

		stale, err := staleInfrastructureRemediationPark(context.Background(), provider, repo, item)
		if err != nil {
			t.Fatalf("staleInfrastructureRemediationPark: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — an absent record is not proof")
		}
	})

	// A pending human decision outranks anything this function can conclude,
	// even when an older infrastructure park is still in the thread.
	t.Run("a needs-human label outranks an older infrastructure park", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(5, "issue 5", needsRemediationLabel, providers.LabelNeedsHuman)
		server.addComment(5, remediationParkCommentPrefix+"gate local-gate returned terminal outcome infra")
		provider := server.newGitHubProvider("token")
		item := providers.WorkItem{ID: "5", Labels: []string{needsRemediationLabel, providers.LabelNeedsHuman}}

		stale, err := staleInfrastructureRemediationPark(context.Background(), provider, repo, item)
		if err != nil {
			t.Fatalf("staleInfrastructureRemediationPark: %v", err)
		}
		if stale {
			t.Fatal("stale = true, want false — a human decision is pending on this item")
		}
	})
}

// The comment reader in isolation, including the ordering rules that decide
// WHICH park is the operative one.
func TestLatestParkIsInfrastructure(t *testing.T) {
	comment := func(body string) providers.Comment { return providers.Comment{Body: body} }

	cases := []struct {
		name     string
		comments []providers.Comment
		want     bool
	}{
		{name: "no comments", comments: nil, want: false},
		{
			name:     "plain terminal infra outcome",
			comments: []providers.Comment{comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infra")},
			want:     true,
		},
		{
			name:     "repass budget exhausted on infra",
			comments: []providers.Comment{comment(remediationParkCommentPrefix + "gate local-gate escalated after outcome infra exhausted the repass budget at attempt 3")},
			want:     true,
		},
		{
			name:     "a different terminal outcome is not infrastructure",
			comments: []providers.Comment{comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome fail")},
			want:     false,
		},
		{
			// "infrastructure" must not satisfy a match for "infra" as an
			// outcome name; the outcome is a fixed vocabulary word.
			name:     "a prose mention of infrastructure is not an outcome",
			comments: []providers.Comment{comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infrastructure-ish")},
			want:     false,
		},
		{
			name: "evidence appended below the reason still parses",
			comments: []providers.Comment{comment(remediationParkCommentPrefix +
				"gate local-gate returned terminal outcome infra\n\n---\n\n**Last review verdict** (gate `review`)…")},
			want: true,
		},
		{
			// A later merit park supersedes an earlier infrastructure one: the
			// item has since had a real attempt and failed it on merit.
			name: "a later merit park supersedes an earlier infra park",
			comments: []providers.Comment{
				comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infra"),
				comment(remediationParkCommentPrefix + "the implementer produced no change in response"),
			},
			want: false,
		},
		{
			name: "a later infra park supersedes an earlier merit park",
			comments: []providers.Comment{
				comment(remediationParkCommentPrefix + "the implementer produced no change in response"),
				comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infra"),
			},
			want: true,
		},
		{
			name: "a later needs-human park stops the scan",
			comments: []providers.Comment{
				comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infra"),
				comment(humanParkCommentPrefix + "the reviewer returned fail: which retention policy applies here?"),
			},
			want: false,
		},
		{
			name: "unrelated comments after the park do not hide it",
			comments: []providers.Comment{
				comment(remediationParkCommentPrefix + "gate local-gate returned terminal outcome infra"),
				comment("goobers-claim: run=f071bf34b79a5fe06a5452a8c0165ac2"),
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := latestParkIsInfrastructure(tc.comments); got != tc.want {
				t.Fatalf("latestParkIsInfrastructure = %v, want %v", got, tc.want)
			}
		})
	}
}
