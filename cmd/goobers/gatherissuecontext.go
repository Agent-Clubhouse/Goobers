package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const gatherIssueContextHelp = "Usage: goobers gather-issue-context [path]\n\n" +
	"Read this run's latest remediation brief, resolve the selected PR's\n" +
	"Fixes/Closes/Resolves issue references, and replace only the brief's\n" +
	"gatherIssueContext section with the originating issue bodies. Missing\n" +
	"PRs, absent references, and referenced issues that no longer resolve\n" +
	"produce an empty issues list rather than failing the remediation cycle.\n" +
	"[path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = issue context\n" +
	"gathered (possibly empty), 1 = business/provider/journal error, 2 =\n" +
	"usage/IO error.\n"

func runGatherIssueContext(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-issue-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-issue-context")
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

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	brief, err := readLatestRemediationBrief(root, runID)
	if err != nil {
		pf(stderr, "error: read remediation brief: %v\n", err)
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
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
	// The PR listing and the originating-issue reads authenticate with
	// distinct capabilities (github:pr:write vs github:issues:write), which
	// per-capability credential overrides may back with different tokens.
	// Use each capability's own provider so issue resolution never fails on a
	// PR-scoped credential.
	prProvider := newCachedGitHubProvider(root, prToken)
	issuesProvider := newCachedGitHubProvider(root, issuesToken)
	ctx, cancel := providerCommandContext()
	defer cancel()

	issues := make([]apiv1.RemediationIssue, 0)
	prs, err := prProvider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		Base:           brief.Base,
		SkipCheckState: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list pull requests", err, remediationBriefResultFile)
	}
	var prBody string
	foundPR := false
	for _, pr := range prs {
		if fmt.Sprint(pr.Number) == brief.SelectedNumber {
			prBody = pr.Body
			foundPR = true
			break
		}
	}
	if !foundPR {
		pf(stderr, "warning: selected PR #%s no longer resolves; emitting empty issue context\n", brief.SelectedNumber)
	} else {
		refs := closingIssueNumbers(prBody)
		issues = make([]apiv1.RemediationIssue, 0, len(refs))
		for _, number := range refs {
			item, issueErr := issuesProvider.GetWorkItem(ctx, repo, number)
			if providers.IsNotFoundError(issueErr) {
				pf(stderr, "warning: originating issue #%s no longer resolves; omitting it from issue context\n", number)
				continue
			}
			if issueErr != nil {
				return failProviderStage(stderr, fmt.Sprintf("read originating issue #%s", number), issueErr, remediationBriefResultFile)
			}
			issues = append(issues, apiv1.RemediationIssue{
				Number: number,
				Title:  item.Title,
				Body:   item.Body,
				URL:    item.URL,
			})
		}
	}

	brief.GatherIssueContext = &apiv1.RemediationIssueContext{Issues: issues}
	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	pf(stdout, "gathered %d originating issue(s) for PR #%s\n", len(issues), brief.SelectedNumber)
	return 0
}

func readLatestRemediationBrief(root, runID string) (apiv1.RemediationBrief, error) {
	runDir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}

	var latest apiv1.RemediationBrief
	found := false
	prefix := runID + ":gather-"
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil ||
			!strings.HasPrefix(event.Name, prefix) || !strings.HasSuffix(event.Name, "/result") {
			continue
		}
		data, readErr := rd.ArtifactBytes(*event.Ref)
		if readErr != nil {
			return apiv1.RemediationBrief{}, fmt.Errorf("read %s: %w", event.Name, readErr)
		}
		var header struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(data, &header) != nil || header.Schema == "" {
			continue
		}
		if !strings.HasPrefix(header.Schema, "goobers.dev/remediation-brief/") {
			continue
		}
		if header.Schema != apiv1.RemediationBriefVersion {
			return apiv1.RemediationBrief{}, fmt.Errorf(
				"%s schema is %q, want %q",
				event.Name, header.Schema, apiv1.RemediationBriefVersion,
			)
		}
		if err := json.Unmarshal(data, &latest); err != nil {
			return apiv1.RemediationBrief{}, fmt.Errorf("unmarshal %s: %w", event.Name, err)
		}
		found = true
	}
	if !found {
		return apiv1.RemediationBrief{}, fmt.Errorf("no remediation-brief artifact found")
	}
	if latest.SelectedNumber == "" {
		return apiv1.RemediationBrief{}, fmt.Errorf("latest remediation brief has no selectedNumber")
	}
	return latest, nil
}
