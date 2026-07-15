package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// siblingPR is one OTHER open PR's evidence for the holistic review — what
// it touches and its own state, so the reviewer can spot cross-PR
// conflict/drift the in-run reviewer (which sees only one diff) never can
// (issue #359, design doc §3).
type siblingPR struct {
	Number     int      `json:"number"`
	URL        string   `json:"url"`
	Head       string   `json:"head"`
	Draft      bool     `json:"draft"`
	Labels     []string `json:"labels,omitempty"`
	CheckState string   `json:"checkState"`
	Files      []string `json:"files"`
}

// runGatherSiblingContext implements `goobers gather-sibling-context`
// (issue #359): loads every OTHER open PR's touched files + state as
// evidence context for the holistic review gate that follows — the
// sibling-set context stage the design doc calls "where the cross-PR value
// lives; without it the review degrades back to single-diff and catches
// nothing cross-cutting." Deliberately queries ALL other open PRs (not just
// ones pr-select would itself find eligible) — a sibling that's draft, red,
// or already labeled is still relevant evidence (e.g. "PR #12 touches the
// same file but isn't ready yet").
func runGatherSiblingContext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-sibling-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers gather-sibling-context [path]\n\n"+
			"Load the other open goober-authored PRs' touched files + state as\n"+
			"evidence for the holistic review (a workflow stage, follows\n"+
			"pr-select). Requires selectedNumber/selectedHead/selectedBase inputs\n"+
			"(Task.InputsFrom pr-select's own outputs). Exit codes: 0 = context\n"+
			"gathered (possibly empty — no siblings is not an error), 1 = business\n"+
			"error, 2 = usage/IO error.\n")
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

	selectedNumberStr := providerInput("selectedNumber", "")
	if selectedNumberStr == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	selectedNumber, err := strconv.Atoi(selectedNumberStr)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", selectedNumberStr, err)
		return 1
	}
	base := providerInput("base", "main")
	headPrefix := providerInput("headPrefix", "goobers/")

	ctx := context.Background()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	})
	if err != nil {
		pf(stderr, "error: list pull requests: %v\n", err)
		return 1
	}

	var selectedHeadSHA, selectedBaseSHA string
	siblings := make([]siblingPR, 0, len(prs))
	for _, pr := range prs {
		if pr.Number == selectedNumber {
			// Capture the selected PR's OWN current SHAs from this same
			// fresh query — this is what the review gate's Verdict should
			// pin against (design doc §6 D6), not whatever pr-select saw
			// several stages ago.
			selectedHeadSHA, selectedBaseSHA = pr.HeadSHA, pr.BaseSHA
			continue
		}
		files, ferr := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
		if ferr != nil {
			pf(stderr, "error: list files for PR #%d: %v\n", pr.Number, ferr)
			return 1
		}
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		siblings = append(siblings, siblingPR{
			Number: pr.Number, URL: pr.URL, Head: pr.Head, Draft: pr.Draft,
			Labels: pr.Labels, CheckState: string(pr.CheckState), Files: paths,
		})
	}

	if selectedHeadSHA == "" {
		// The selected PR vanished from the eligible list between pr-select
		// and here (merged/closed/retargeted mid-cycle) — nothing to review.
		return writeNoWorkResult(stdout, stderr, "selected PR is no longer open")
	}

	resultFile := providerInput("resultFile", "sibling-context.json")
	data, err := json.MarshalIndent(map[string]interface{}{
		"selectedNumber":  selectedNumber,
		"selectedHeadSha": selectedHeadSHA,
		"selectedBaseSha": selectedBaseSHA,
		"siblings":        siblings,
	}, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal sibling context: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	pf(stdout, "gathered context for %d sibling PR(s)\n", len(siblings))
	return 0
}
