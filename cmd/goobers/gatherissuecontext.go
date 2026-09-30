package main

import (
	"context"
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
	fs := newCLIFlagSet("gather-issue-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-issue-context")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

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
	// Originating issues are read on the routed repository, or on the backlog
	// provider when the backlog lives on another provider (topology (b)).
	issuesRepo := issueContextIssuesRepo(root, repo)
	source, err := newIssueContextSource(root, repo, issuesRepo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()

	issues := make([]apiv1.RemediationIssue, 0)
	integrities := []apiv1.Integrity{brief.Integrity}
	pr, foundPR, operation, err := source.selectedPR(ctx, brief)
	if err != nil {
		return failProviderStage(stderr, operation, err, remediationBriefResultFile)
	}
	if !foundPR {
		pf(stderr, "warning: selected PR #%s no longer resolves; emitting empty issue context\n", brief.SelectedNumber)
	} else {
		integrities = append(integrities, pr.Integrity)
		refs := postMergeClosingIDs(pr.Body, repo, issuesRepo)
		issues = make([]apiv1.RemediationIssue, 0, len(refs))
		for _, number := range refs {
			item, issueErr := source.issues.GetWorkItem(ctx, issuesRepo, number)
			if providers.IsNotFoundError(issueErr) {
				pf(stderr, "warning: originating issue #%s no longer resolves; omitting it from issue context\n", number)
				continue
			}
			if issueErr != nil {
				return failProviderStage(stderr, fmt.Sprintf("read originating issue #%s", number), issueErr, remediationBriefResultFile)
			}
			issues = append(issues, apiv1.RemediationIssue{
				Number:    number,
				Title:     item.Title,
				Body:      item.Body,
				URL:       item.URL,
				Integrity: item.Integrity,
			})
			integrities = append(integrities, item.Integrity)
		}
	}

	brief.GatherIssueContext = &apiv1.RemediationIssueContext{Issues: issues}
	brief.Integrity = apiv1.WeakestIntegrity(integrities...)
	resultFile := providerInput("resultFile", remediationBriefResultFile)
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation brief: %v\n", err)
		return 1
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		pf(stderr, "error: %v\n", err)
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
	const upstream = "gather-pr-context"
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(upstream, remediationBriefArtifact, err)
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(upstream, remediationBriefArtifact, err)
	}

	var latest apiv1.RemediationBrief
	found := false
	for _, event := range events {
		// stageArtifactName, not a hard-coded "<runID>:" prefix: a pod records
		// the same artifact without the run qualifier (#4119).
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil ||
			!strings.HasPrefix(stageArtifactStage(runID, event.Name), "gather-") ||
			!strings.HasSuffix(event.Name, "/result") {
			continue
		}
		data, readErr := rd.ArtifactBytes(*event.Ref)
		if readErr != nil {
			return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(upstream, remediationBriefArtifact,
				fmt.Errorf("read %s: %w", event.Name, readErr))
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
		if !apiv1.SupportedRemediationBriefVersion(header.Schema) {
			return apiv1.RemediationBrief{}, fmt.Errorf(
				"%s schema is %q, want one of %s",
				event.Name, header.Schema, strings.Join(apiv1.SupportedRemediationBriefVersions(), ", "),
			)
		}
		if err := json.Unmarshal(data, &latest); err != nil {
			return apiv1.RemediationBrief{}, fmt.Errorf("unmarshal %s: %w", event.Name, err)
		}
		// A brief written by an older gatherer predates the TBH-4 provenance
		// fields, so they decode as empty. Default them to unapproved rather
		// than rejecting the artifact: a run that produced a v1/v2 brief before
		// this binary deployed must still be able to resume, and an unlabeled
		// grade must fail closed at admission rather than pass as trusted.
		latest = apiv1.MigrateRemediationBrief(latest, header.Schema)
		found = true
	}
	if !found {
		return apiv1.RemediationBrief{}, upstreamArtifactMissing(upstream, remediationBriefArtifact)
	}
	if latest.SelectedNumber == "" {
		return apiv1.RemediationBrief{}, fmt.Errorf("latest remediation brief has no selectedNumber")
	}
	return latest, nil
}

// issueContextIssuesRepo is where gather-issue-context reads originating
// issues: the routed repository, or the backlog provider in topology (b). On
// Azure DevOps work items live in the gaggle's backlog project, which may
// differ from the routed code repository's project (design
// ado-parity-dsl-2-0.md §6), exactly as post-merge addresses them.
func issueContextIssuesRepo(root string, repo providers.RepositoryRef) providers.RepositoryRef {
	backlog := backlogRepoRefForStage(root, repo)
	if repo.Provider == providers.ProviderADO {
		return backlog
	}
	return backlogProviderRepo(repo, backlog)
}

// issueContextIssueReader is gather-issue-context's originating-issue read.
type issueContextIssueReader interface {
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
}

// issueContextPullRequestReader is gather-issue-context's single pull-request
// read on Azure DevOps.
type issueContextPullRequestReader interface {
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
}

// issueContextSource is the provider access gather-issue-context needs: the
// selected pull request (its body names the originating issues) and the
// originating issues themselves. selectedPR reports whether the brief's pull
// request still resolves as an open pull request into the brief's base, and
// on error names the failed step for failProviderStage.
type issueContextSource struct {
	selectedPR func(ctx context.Context, brief apiv1.RemediationBrief) (providers.PullRequestSummary, bool, string, error)
	issues     issueContextIssueReader
}

// newIssueContextSource builds each provider from its own declared
// capability: github:pr:write for the pull request and github:issues:read for
// the originating issues, which per-capability credential overrides may back
// with different tokens, so issue resolution never fails on a PR-scoped
// credential. GitHub and Gitea use the broad remediation factory as before.
// Azure DevOps (whose *ADOProvider does not implement that surface) builds
// narrow surfaces through remediationStageSurface; its pull-request list
// carries no description, so the selected pull request is read on its own.
func newIssueContextSource(root string, repo, issuesRepo providers.RepositoryRef) (issueContextSource, error) {
	prToken, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return issueContextSource{}, err
	}
	issuesToken, err := providerToken(capability.GitHubIssuesRead)
	if err != nil {
		return issueContextSource{}, err
	}
	var source issueContextSource
	if repo.Provider == providers.ProviderADO {
		reader, err := remediationStageSurface[issueContextPullRequestReader](root, repo, prToken, withStageProviderCapability(capability.GitHubPRWrite))
		if err != nil {
			return issueContextSource{}, err
		}
		source.selectedPR = adoIssueContextSelectedPR(reader, repo)
	} else {
		prProvider, err := remediationStageProvider(root, repo, prToken, true)
		if err != nil {
			return issueContextSource{}, err
		}
		source.selectedPR = listedIssueContextSelectedPR(prProvider, repo)
	}
	source.issues, err = issueContextIssues(root, issuesRepo, issuesToken)
	if err != nil {
		return issueContextSource{}, err
	}
	return source, nil
}

// issueContextIssues builds the originating-issue reader on issuesRepo's own
// provider: the broad remediation factory on GitHub and Gitea, a narrow
// surface on Azure DevOps.
func issueContextIssues(root string, issuesRepo providers.RepositoryRef, token string) (issueContextIssueReader, error) {
	if issuesRepo.Provider == providers.ProviderADO {
		return remediationStageSurface[issueContextIssueReader](root, issuesRepo, token, withStageProviderCapability(capability.GitHubIssuesRead))
	}
	return remediationStageProvider(root, issuesRepo, token, true)
}

// listedIssueContextSelectedPR finds the brief's pull request among the open
// pull requests into its base, as gather-issue-context always has on GitHub
// and Gitea.
func listedIssueContextSelectedPR(provider remediationProvider, repo providers.RepositoryRef) func(context.Context, apiv1.RemediationBrief) (providers.PullRequestSummary, bool, string, error) {
	return func(ctx context.Context, brief apiv1.RemediationBrief) (providers.PullRequestSummary, bool, string, error) {
		prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
			Repository:     repo,
			Base:           brief.Base,
			SkipCheckState: true,
		})
		if err != nil {
			return providers.PullRequestSummary{}, false, "list pull requests", err
		}
		for _, pr := range prs {
			if fmt.Sprint(pr.Number) == brief.SelectedNumber {
				return pr, true, "", nil
			}
		}
		return providers.PullRequestSummary{}, false, "", nil
	}
}

// adoIssueContextSelectedPR reads the brief's pull request directly: an Azure
// DevOps pull-request list omits the description that carries the closing
// references. A pull request that is gone, no longer active, or now targets
// another base does not resolve, matching the open-into-base list the other
// providers search.
func adoIssueContextSelectedPR(reader issueContextPullRequestReader, repo providers.RepositoryRef) func(context.Context, apiv1.RemediationBrief) (providers.PullRequestSummary, bool, string, error) {
	return func(ctx context.Context, brief apiv1.RemediationBrief) (providers.PullRequestSummary, bool, string, error) {
		pr, err := reader.GetPullRequest(ctx, repo, brief.SelectedNumber)
		if providers.IsNotFoundError(err) {
			return providers.PullRequestSummary{}, false, "", nil
		}
		if err != nil {
			return providers.PullRequestSummary{}, false, fmt.Sprintf("get pull request #%s", brief.SelectedNumber), err
		}
		if pr.State != "open" || (brief.Base != "" && pr.Base != brief.Base) {
			return providers.PullRequestSummary{}, false, "", nil
		}
		return pr, true, "", nil
	}
}
