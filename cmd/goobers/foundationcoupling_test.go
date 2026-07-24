package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
	"github.com/goobers/goobers/providers"
)

func TestDetectFoundationCouplings(t *testing.T) {
	pr := func(number int, files ...providers.ChangedFile) pullRequestDiffProfile {
		return newPullRequestDiffProfile(providers.PullRequestSummary{Number: number}, files)
	}
	rewrite := func(number, before, after int, file providers.ChangedFile) pullRequestDiffProfile {
		profile := pr(number, file)
		profile.lineCounts[file.Path] = fileLineCounts{before: before, after: after}
		return profile
	}
	narrow := providers.ChangedFile{
		Path: "App.tsx", Status: "modified", Additions: 6, Deletions: 4,
		Patch: "@@ -100,4 +100,6 @@",
	}
	tests := []struct {
		name        string
		dependents  []pullRequestDiffProfile
		foundations []pullRequestDiffProfile
		blocked     map[int]bool
		want        int
	}{
		{
			name:       "deleted shared file",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{Path: "App.tsx", Status: "removed", Deletions: 1400}),
			},
			want: 709,
		},
		{
			name:       "renamed shared file",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{
					Path: "AppShell.tsx", PreviousPath: "App.tsx", Status: "renamed",
				}),
			},
			want: 709,
		},
		{
			name:       "order of magnitude shrink",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(709, 1400, 32, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 32, Deletions: 1400,
				}),
			},
			want: 709,
		},
		{
			name:       "majority rewrite",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(709, 500, 520, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 320, Deletions: 300,
					Patch: "@@ -1,500 +1,520 @@",
				}),
			},
			want: 709,
		},
		{
			name:       "below absolute floor",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(709, 100, 20, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 10, Deletions: 90,
					Patch: "@@ -1,90 +1,10 @@",
				}),
			},
		},
		{
			name:       "ambiguous rewrites remain unflagged",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(708, 500, 20, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 20, Deletions: 500,
					Patch: "@@ -1,500 +1,20 @@",
				}),
				rewrite(709, 500, 20, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 20, Deletions: 500,
					Patch: "@@ -1,500 +1,20 @@",
				}),
			},
		},
		{
			name:       "blocked foundation remains ineligible",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				pr(709, providers.ChangedFile{Path: "App.tsx", Status: "removed", Deletions: 1400}),
			},
			blocked: map[int]bool{709: true},
		},
		{
			name:       "partial deletion in large file is not a rewrite",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(709, 5000, 4900, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Deletions: 100,
					Patch: "@@ -1,100 +1,0 @@",
				}),
			},
		},
		{
			name:       "early changes in large file are not a majority rewrite",
			dependents: []pullRequestDiffProfile{pr(700, narrow)},
			foundations: []pullRequestDiffProfile{
				pr(700, narrow),
				rewrite(709, 5000, 5000, providers.ChangedFile{
					Path: "App.tsx", Status: "modified", Additions: 500, Deletions: 500,
					Patch: "@@ -1,500 +1,500 @@",
				}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectFoundationCouplings(tc.dependents, tc.foundations, tc.blocked)
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
	server.setFileContent("h709", "portal/src/App.tsx", strings.Repeat("new\n", 32))

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

func TestPRSelectFoundationScanIncludesNonGooberPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addFoundationScanFixture(server, "human/foundation")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked PR #700 behind PR #709") {
		t.Fatalf("stdout = %q, want non-goober foundation detected", stdout)
	}
	assertFoundationBlocker(t, server, 700, 709)
}

func TestPRSelectFoundationScanIncludesWebhookSiblings(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addFoundationScanFixture(server, "goobers/implementation/709")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
		Event:      "pull_request",
		PullNumber: 700,
	}))
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked PR #700 behind PR #709") {
		t.Fatalf("stdout = %q, want webhook sibling foundation detected", stdout)
	}
	assertFoundationBlocker(t, server, 700, 709)
}

func TestPRSelectFoundationScanDetectsRenameAway(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, number := range []int{700, 709} {
		server.addIssue(number, "PR "+strconv.Itoa(number))
	}
	server.addOpenPR(700, "goobers/implementation/700", "main", "h700", "base", false, nil, []fakePRFile{{
		path: "portal/src/App.tsx", status: "modified", additions: 6, deletions: 4,
	}})
	server.addOpenPR(709, "goobers/implementation/709", "main", "h709", "base", false, nil, []fakePRFile{{
		path: "portal/src/AppShell.tsx", previousPath: "portal/src/App.tsx", status: "renamed",
	}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-rename-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked PR #700 behind PR #709") {
		t.Fatalf("stdout = %q, want rename-away foundation detected", stdout)
	}
	assertFoundationBlocker(t, server, 700, 709)
}

func TestPRSelectFoundationScanIncludesSelfHealedLifecycleLabels(t *testing.T) {
	tests := []struct {
		name  string
		label string
	}{
		{name: "escalation", label: remediationEscalatedLabel},
		{name: "demotion", label: mergeDemotedLabel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			addFoundationScanFixture(server, "goobers/implementation/709", tc.label)
			var comment string
			var err error
			switch tc.label {
			case remediationEscalatedLabel:
				comment, err = remediationStateComment(remediationState{
					Escalated: true, EscalatedHeadSHA: "old-h709", EscalatedBaseSHA: "base",
				})
			case mergeDemotedLabel:
				comment, err = mergeDemotionComment(mergeDemotionState{
					Demoted: true, HeadSHA: "old-h709",
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			server.addComment(709, comment)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-healed-"+tc.name)
			t.Setenv("GOOBERS_WORKFLOW", "merge-review")
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "pr-select", root)
			if code != 0 {
				t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "parked PR #700 behind PR #709") {
				t.Fatalf("stdout = %q, want self-healed %s foundation detected", stdout, tc.name)
			}
			if got := selectedPullNumber(t, "selected-pr.json"); got != "709" {
				t.Fatalf("selected PR = %s, want self-healed foundation PR #709", got)
			}
			assertFoundationBlocker(t, server, 700, 709)
			if tc.label == mergeDemotedLabel {
				provider := server.newGitHubProvider("token")
				filtered, err := filterRemediationPullRequests(
					context.Background(),
					provider,
					providers.RepositoryRef{Owner: "your-org", Name: "your-repo"},
					[]providers.PullRequestSummary{{
						Number: 700,
						Labels: []string{needsRemediationLabel, blockedOnSiblingLabel},
					}},
					nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				if len(filtered) != 0 {
					t.Fatalf("remediation candidates = %+v, want dependent held behind self-healed demoted foundation #709", filtered)
				}
			}
		})
	}
}

func TestPRSelectFoundationScanIncludesDependentsWhenFoundationWebhookTriggers(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addFoundationScanFixture(server, "goobers/implementation/709")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv(executor.TriggerRefEnvVar, webhookhttp.TriggerRef(webhookhttp.Delivery{
		Event:      "pull_request",
		PullNumber: 709,
	}))
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "parked PR #700 behind PR #709") {
		t.Fatalf("stdout = %q, want existing dependent detected from foundation webhook", stdout)
	}
	assertFoundationBlocker(t, server, 700, 709)
}

func TestPRSelectFoundationScanSkipsUnavailableContent(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	addFoundationScanFixture(server, "goobers/implementation/709")
	server.deleteFileContent("h709", "portal/src/App.tsx")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-foundation")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "warning: foundation-coupling scan:") {
		t.Fatalf("stderr = %q, want visible content warning", stderr)
	}
	if issue := server.issues[700]; hasAnyLabel(issue.labels, []string{blockedOnSiblingLabel}) || len(issue.comments) != 0 {
		t.Fatalf("PR #700 = %+v, want unavailable rewrite evidence left unflagged", issue)
	}
}

func addFoundationScanFixture(server *fakeGitHubServer, foundationHead string, foundationLabels ...string) {
	server.addIssue(700, "PR 700")
	server.addIssue(709, "PR 709", foundationLabels...)
	server.addOpenPR(700, "goobers/implementation/700", "main", "h700", "base", false, nil, []fakePRFile{{
		path: "portal/src/App.tsx", status: "modified", additions: 6, deletions: 4,
		patch: "@@ -100,4 +100,6 @@",
	}})
	server.addOpenPR(709, foundationHead, "main", "h709", "base", false, foundationLabels, []fakePRFile{{
		path: "portal/src/App.tsx", status: "modified", additions: 32, deletions: 1400,
		patch: "@@ -1,1400 +1,32 @@",
	}})
	server.setFileContent("h709", "portal/src/App.tsx", strings.Repeat("new\n", 32))
}

func assertFoundationBlocker(t *testing.T, server *fakeGitHubServer, dependent, foundation int) {
	t.Helper()
	issue := server.issues[dependent]
	if !hasAnyLabel(issue.labels, []string{blockedOnSiblingLabel}) {
		t.Fatalf("PR #%d labels = %v, want %s", dependent, issue.labels, blockedOnSiblingLabel)
	}
	if len(issue.comments) != 1 {
		t.Fatalf("PR #%d comments = %v, want one foundation hold", dependent, issue.comments)
	}
	state, ok := parseBlockedOnSiblingComment(issue.comments[0])
	if !ok || len(state.Blockers) != 1 || state.Blockers[0] != foundation {
		t.Fatalf("PR #%d blocked state = %+v, ok = %t; want named blocker #%d", dependent, state, ok, foundation)
	}
}
