package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

// checkIssueStalenessHelp documents `goobers check-issue-staleness` (#2340):
// merge-review's pre-review staleness gate. Sits between pr-select and
// gather-sibling-context — before the expensive agentic review gate, the
// same altitude open-pr's #947 mid-flight staleness re-check runs at one
// stage earlier in implementation.yaml.
const checkIssueStalenessHelp = "Usage: goobers check-issue-staleness [path]\n\n" +
	"Re-fetch the PR's pinned linked issue and compare its title and body\n" +
	"against the snapshot taken when the PR was opened. If the issue spec\n" +
	"changed since implementation began, label the PR goobers:needs-\n" +
	"remediation and post an explanatory comment instead of letting review\n" +
	"proceed against stale copied criteria. A PR with no pin (predates this\n" +
	"feature, or its linked issue never resolved an updatedAt) is never\n" +
	"considered stale — there is nothing to compare against. Declared\n" +
	"inputs: pullNumber (required), head/base/advisoryMode (passed through\n" +
	"unchanged for the next stage's inputsFrom). Writes\n" +
	"issueStale/number/head/base/advisoryMode to the declared result file.\n" +
	"Exit codes: 0 = evaluated, 1 = business error, 2 = usage/IO error.\n"

func runCheckIssueStaleness(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("check-issue-staleness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "check-issue-staleness")
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
	pullNumber := providerInput("pullNumber", "")
	if pullNumber == "" {
		pf(stderr, "error: pullNumber is required (inputsFrom pr-select's number output)\n")
		return 1
	}
	head := providerInput("head", "")
	base := providerInput("base", "")
	advisoryMode := providerInput("advisoryMode", "false")
	resultFile := providerInput("resultFile", "issue-staleness-result.json")

	// The PR poll and the originating-issue read authenticate with distinct
	// capabilities (github:pr:write vs github:issues:write), the same split
	// gather-issue-context uses, so issue resolution never fails on a
	// PR-scoped credential and vice versa.
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issuesToken, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	prProvider := newCachedGitHubProvider(root, prToken)
	issuesProvider := newCachedGitHubProvider(root, issuesToken)

	ctx, cancel := providerCommandContext()
	defer cancel()

	poll, err := prProvider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: repo, PullID: pullNumber})
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("poll pull request #%s", pullNumber), err, resultFile)
	}

	pin, havePin := parseIssueSpecPin(poll.Body)
	stale := false
	var reason string
	var refreshedUpdatedAt string
	var refreshedTitle string
	var refreshedBody string
	if havePin {
		pinnedAt, parseErr := time.Parse(time.RFC3339, pin.UpdatedAt)
		if parseErr != nil && pin.SpecDigest == "" {
			pf(stderr, "warning: pr #%s issue-spec pin has an unparseable updatedAt %q; treating as not stale\n", pullNumber, pin.UpdatedAt)
		} else {
			item, issueErr := issuesProvider.GetWorkItem(ctx, repo, pin.IssueID)
			switch {
			case providers.IsNotFoundError(issueErr):
				pf(stdout, "pinned issue #%s no longer resolves; treating pr #%s as not stale\n", pin.IssueID, pullNumber)
			case issueErr != nil:
				return failProviderStage(stderr, fmt.Sprintf("read pinned issue #%s", pin.IssueID), issueErr, resultFile)
			default:
				refreshedTitle = item.Title
				refreshedBody = item.Body
				if item.UpdatedAt != nil {
					refreshedUpdatedAt = item.UpdatedAt.Format(time.RFC3339)
				} else {
					refreshedUpdatedAt = pin.UpdatedAt
				}
				if pin.SpecDigest != "" {
					stale = issueSpecDigest(item.Title, item.Body) != pin.SpecDigest
				} else {
					stale = item.UpdatedAt != nil && item.UpdatedAt.After(pinnedAt)
				}
				if stale {
					reason = fmt.Sprintf(
						"issue #%s's title or body changed since this PR's implementation-time snapshot — routing to remediation instead of reviewing stale criteria",
						pin.IssueID,
					)
				}
			}
		}
	}

	if stale {
		// Advance the pin to the edit just observed, in the same update that
		// posts the label/comment. Without this, no stage ever rewrites the
		// marker again (open-pr, the only other writer, belongs to
		// implementation.yaml and never runs a second time for an existing
		// PR), so every future check-issue-staleness run keeps re-comparing
		// against this same original snapshot and re-fires on the identical
		// already-reported edit forever, even after remediation responds.
		updatedPRBody := replaceIssueSpecPin(poll.Body, pin.IssueID, refreshedUpdatedAt, refreshedTitle, refreshedBody)
		if _, err := prProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: pullNumber,
			AddLabels: []string{needsRemediationLabel},
			Comment:   "**Issue spec changed since implementation began (#2340)**\n\n" + reason,
			Body:      &updatedPRBody,
		}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("label pr #%s for issue-spec staleness", pullNumber), err, resultFile)
		}
	}

	out := map[string]interface{}{
		"number":       pullNumber,
		"head":         head,
		"base":         base,
		"advisoryMode": advisoryMode,
		"issueStale":   strconv.FormatBool(stale),
	}
	if reason != "" {
		out["reason"] = reason
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal issue-staleness result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	if stale {
		pf(stdout, "pr #%s: %s\n", pullNumber, reason)
	} else {
		pf(stdout, "pr #%s: issue spec not stale\n", pullNumber)
	}
	return 0
}
