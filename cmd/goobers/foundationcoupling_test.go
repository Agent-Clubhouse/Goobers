package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestDetectFoundationCouplings(t *testing.T) {
	pr := func(number int, files ...providers.ChangedFile) pullRequestDiffProfile {
		return newPullRequestDiffProfile(providers.PullRequestSummary{Number: number}, files)
	}
	narrow := providers.ChangedFile{
		Path: "App.tsx", Status: "modified", Additions: 6, Deletions: 4,
		Patch: "@@ -100,4 +100,6 @@",
	}
	tests := []struct {
		name     string
		profiles []pullRequestDiffProfile
		blocked  map[int]bool
		want     int
	}{
		{
			name: "deleted shared file",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{Path: "App.tsx", Status: "removed", Deletions: 1400}),
			},
			want: 709,
		},
		{
			name: "order of magnitude shrink",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 32, Deletions: 1400,
				}),
			},
			want: 709,
		},
		{
			name: "majority rewrite",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 320, Deletions: 300,
					Patch: "@@ -1,500 +1,520 @@",
				}),
			},
			want: 709,
		},
		{
			name: "below absolute floor",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 10, Deletions: 90,
					Patch: "@@ -1,90 +1,10 @@",
				}),
			},
		},
		{
			name: "ambiguous rewrites remain unflagged",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(708, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 20, Deletions: 500,
					Patch: "@@ -1,500 +1,20 @@",
				}),
				pr(709, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 20, Deletions: 500,
					Patch: "@@ -1,500 +1,20 @@",
				}),
			},
		},
		{
			name: "blocked foundation remains ineligible",
			profiles: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{Path: "App.tsx", Status: "removed", Deletions: 1400}),
			},
			blocked: map[int]bool{709: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectFoundationCouplings(tc.profiles, tc.blocked)
			if tc.want == 0 {
				if len(got) != 0 {
					t.Fatalf("couplings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].dependent.Number != 700 || got[0].foundation.Number != tc.want {
				t.Fatalf("couplings = %+v, want PR #700 behind #%d", got, tc.want)
			}
		})
	}
}

func TestPRSelectFlagsFoundationCouplingBeforeRemediation(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, number := range []int{700, 701, 709} {
		server.addIssue(number, "PR "+strconv.Itoa(number))
	}
	narrow := []fakePRFile{{
		path: "portal/src/App.tsx", status: "modified", additions: 6, deletions: 4,
		patch: "@@ -100,4 +100,6 @@",
	}}
	server.addOpenPR(700, "goobers/implementation/700", "main", "h700", "base", false, nil, narrow)
	server.addOpenPR(701, "goobers/implementation/701", "main", "h701", "base", false, []string{needsRemediationLabel}, narrow)
	server.addOpenPR(709, "goobers/implementation/709", "main", "h709", "base", false, nil, []fakePRFile{
		{
			path: "portal/src/App.tsx", status: "modified", additions: 32, deletions: 1400,
			patch: "@@ -1,1400 +1,32 @@",
		},
		{path: "portal/src/routing.ts", status: "added", additions: 200, patch: "@@ -0,0 +1,200 @@"},
	})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked PR #700 behind PR #709") ||
		!strings.Contains(stdout, "parked PR #701 behind PR #709") {
		t.Fatalf("stdout = %q, want both narrower PRs visibly parked", stdout)
	}
	if got := selectedPullNumber(t, "selected-pr.json"); got != "709" {
		t.Fatalf("selected PR = %s, want foundation PR #709", got)
	}

	for _, number := range []int{700, 701} {
		issue := server.issues[number]
		if !hasAnyLabel(issue.labels, []string{blockedOnSiblingLabel}) {
			t.Fatalf("PR #%d labels = %v, want %s", number, issue.labels, blockedOnSiblingLabel)
		}
		if len(issue.comments) != 1 || !strings.Contains(issue.comments[0], "Foundation-coupled hold") {
			t.Fatalf("PR #%d comments = %v, want one visible foundation hold", number, issue.comments)
		}
		state, ok := parseBlockedOnSiblingComment(issue.comments[0])
		if !ok || len(state.Blockers) != 1 || state.Blockers[0] != 709 {
			t.Fatalf("PR #%d blocked state = %+v, ok = %t; want named blocker #709", number, state, ok)
		}
	}

	dependent := providers.PullRequestSummary{
		Number: 701,
		Labels: []string{needsRemediationLabel, blockedOnSiblingLabel},
	}
	provider := server.newGitHubProvider("token")
	filtered, err := filterRemediationPullRequests(context.Background(), provider, providers.RepositoryRef{
		Owner: "your-org", Name: "your-repo",
	}, []providers.PullRequestSummary{dependent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("remediation candidates = %+v, want foundation-coupled PR held while #709 is open", filtered)
	}

	server.closeIssue(709)
	filtered, err = filterRemediationPullRequests(context.Background(), provider, providers.RepositoryRef{
		Owner: "your-org", Name: "your-repo",
	}, []providers.PullRequestSummary{dependent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidates, priority, err := selectRemediationCandidates(filtered, func(providers.PullRequestSummary) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Number != 701 || priority != remediationPriorityNeedsRemediation {
		t.Fatalf("after foundation closes, candidates = %+v priority = %v; want PR #701 eligible for agentic re-derivation", candidates, priority)
	}
}
