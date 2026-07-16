package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// defaultExcludeLabels are the labels that mean "already decided, don't
// re-review" (design doc §3): a PR merge-review already verdicted this cycle
// carries one of these until pr-remediation/auto-merge acts on it and clears
// it. Re-selecting it would waste a cycle re-reviewing something already in
// flight — harmless under G3, but pointless.
const defaultExcludeLabels = "goobers:merge-ready,goobers:needs-remediation,goobers:merge-escalated"

// runPRSelect implements `goobers pr-select` (issue #359): merge-review's
// selection stage. Picks at most one eligible PR per run — the same
// one-per-run shape backlog-query uses for issues (design doc §3's
// declarative-selection model), not a batch scan of the whole open-PR set in
// a single run. The selected PR is leased in the shared PR claim namespace so
// concurrent merge-review and pr-remediation runs cannot select it together.
func runPRSelect(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr-select", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers pr-select [path]\n\n"+
			"Select at most one open, non-draft, green-CI goober-authored PR for\n"+
			"merge-review to evaluate this cycle (a workflow stage). Writes the\n"+
			"selected PR's number/head/base/headSha/baseSha/url to the declared\n"+
			"result file. Exit codes: 0 = selected (or no-work), 1 = business error,\n"+
			"2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)

	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")
	excludeLabels := strings.Split(providerInput("excludeLabels", defaultExcludeLabels), ",")

	ctx := context.Background()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		pf(stderr, "error: list pull requests: %v\n", err)
		return 1
	}

	var eligible []providers.PullRequestSummary
	for _, pr := range prs {
		if pr.Draft {
			continue
		}
		if pr.CheckState != providers.CheckStatePassing {
			continue
		}
		if hasAnyLabel(pr.Labels, excludeLabels) {
			continue
		}
		eligible = append(eligible, pr)
	}
	if len(eligible) == 0 {
		return writeNoWorkResult(stdout, stderr, "no eligible PR to select this cycle")
	}

	claimed, err := claimEligiblePullRequest(root, eligible)
	if err != nil {
		pf(stderr, "error: claim eligible PR: %v\n", err)
		return 1
	}
	if claimed == nil {
		return writeNoWorkResult(stdout, stderr, "every eligible PR is already claimed by another run")
	}
	selected := *claimed

	resultFile := providerInput("resultFile", "selected-pr.json")
	data, err := json.Marshal(map[string]string{
		"number":  strconv.Itoa(selected.Number),
		"head":    selected.Head,
		"base":    selected.Base,
		"headSha": selected.HeadSHA,
		"baseSha": selected.BaseSHA,
		"url":     selected.URL,
	})
	if err != nil {
		pf(stderr, "error: marshal selected PR: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "selected PR #%d: %s\n", selected.Number, selected.URL)
	return 0
}

// hasAnyLabel reports whether labels contains any of wants (case-sensitive,
// matching GitHub's own label-name comparison).
func hasAnyLabel(labels, wants []string) bool {
	for _, w := range wants {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for _, l := range labels {
			if l == w {
				return true
			}
		}
	}
	return false
}
