package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	findingResponsesOutput          = "findingResponses"
	remediationResponseArtifactName = "remediation-response.json"
)

const respondToFindingsHelp = "Usage: goobers respond-to-findings [path]\n\n" +
	"Read the claimed PR's original remediation verdict and the latest\n" +
	"implement stage's findingResponses output from this run's journal.\n" +
	"Require exactly one addressed/declined disposition with a non-empty\n" +
	"detail for every finding, post the resulting changelog to the PR, and\n" +
	"write the complete structured response to the declared result file.\n" +
	"Retries reconcile one run-scoped comment instead of appending duplicates.\n" +
	"If push-remediated skipped a closed PR, records the unposted account\n" +
	"without claiming those local changes landed.\n" +
	"[path] defaults to GOOBERS_INSTANCE_ROOT. Exit codes: 0 = response\n" +
	"processed, 1 = business error, 2 = usage/IO error.\n"

type findingDisposition struct {
	Finding     int    `json:"finding"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type recordedFindingDisposition struct {
	Finding     int           `json:"finding"`
	Original    apiv1.Finding `json:"original"`
	Disposition string        `json:"disposition"`
	Detail      string        `json:"detail"`
}

type remediationResponseResult struct {
	SelectedNumber string                       `json:"selectedNumber"`
	SourceRunID    string                       `json:"sourceRunId"`
	FindingCount   int                          `json:"findingCount"`
	Posted         bool                         `json:"posted"`
	Reason         string                       `json:"reason,omitempty"`
	Findings       []recordedFindingDisposition `json:"findings"`
}

type remediationContextArtifact struct {
	Verdict *apiv1.Verdict `json:"verdict"`
}

func runRespondToFindings(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("respond-to-findings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "respond-to-findings")
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
	selectedNumber, ok, err := claimedPullRequestNumber(root)
	if err != nil {
		pf(stderr, "error: resolve claimed PR: %v\n", err)
		return 1
	}
	if !ok {
		pf(stderr, "error: run holds no PR claim, so there is no remediation thread to respond to\n")
		return 1
	}

	verdict, rawResponses, published, err := readRemediationResponseInputs(root, runID)
	if err != nil {
		pf(stderr, "error: read remediation response inputs from journal: %v\n", err)
		return 1
	}
	responses, err := validateFindingResponses(verdict.Findings, rawResponses)
	if err != nil {
		pf(stderr, "error: validate %s: %v\n", findingResponsesOutput, err)
		return 1
	}

	result := remediationResponseResult{
		SelectedNumber: strconv.Itoa(selectedNumber),
		SourceRunID:    runID,
		FindingCount:   len(verdict.Findings),
		Findings:       make([]recordedFindingDisposition, len(responses)),
	}
	for i, response := range responses {
		result.Findings[i] = recordedFindingDisposition{
			Finding:     response.Finding,
			Original:    verdict.Findings[response.Finding-1],
			Disposition: response.Disposition,
			Detail:      response.Detail,
		}
	}
	if !published {
		result.Reason = "push-remediated skipped publication because the PR was already closed"
		if code := writeRemediationResponseResult(result, stderr); code != 0 {
			return code
		}
		pf(stdout, "PR #%d: remediated branch was not published, so no finding response was posted\n", selectedNumber)
		return 0
	}
	comment := renderRemediationResponse(runID, result)

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	provider := newGitHubProvider(token)
	ctx, cancel := providerCommandContext()
	defer cancel()
	if err := reconcileRemediationResponseComment(ctx, provider, repo, selectedNumber, runID, comment); err != nil {
		return failProviderStage(stderr, fmt.Sprintf("post remediation response to PR #%d", selectedNumber), err, remediationResponseArtifactName)
	}
	result.Posted = true
	if code := writeRemediationResponseResult(result, stderr); code != 0 {
		return code
	}
	pf(stdout, "PR #%d: posted remediation response accounting for %d finding(s)\n", selectedNumber, len(result.Findings))
	return 0
}

func writeRemediationResponseResult(result remediationResponseResult, stderr io.Writer) int {
	resultFile := providerInput("resultFile", remediationResponseArtifactName)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal remediation response: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	return 0
}

func readRemediationResponseInputs(root, runID string) (apiv1.Verdict, string, bool, error) {
	runDir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return apiv1.Verdict{}, "", false, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return apiv1.Verdict{}, "", false, err
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.Verdict{}, "", false, err
	}

	var contextRef *journal.Ref
	var rawResponses string
	var implementFound bool
	var pushFound bool
	var published string
	for i := range events {
		event := events[i]
		if event.Type == journal.EventArtifactRecorded &&
			event.Name == runID+":gather-pr-context/result" &&
			event.Ref != nil {
			ref := *event.Ref
			contextRef = &ref
		}
		if event.Type == journal.EventStageFinished && event.Stage == "implement" {
			implementFound = true
			rawResponses = ""
			if raw, ok := event.Outputs[findingResponsesOutput].(string); ok {
				rawResponses = raw
			}
		}
		if event.Type == journal.EventStageFinished && event.Stage == "push-remediated" {
			pushFound = true
			published, _ = event.Outputs[pushRemediatedPublishedOutput].(string)
		}
	}
	if contextRef == nil {
		return apiv1.Verdict{}, "", false, fmt.Errorf("gather-pr-context produced no pr-context.json artifact")
	}
	if !implementFound {
		return apiv1.Verdict{}, "", false, fmt.Errorf("no implement stage result found")
	}
	if !pushFound {
		return apiv1.Verdict{}, "", false, fmt.Errorf("no push-remediated stage result found")
	}
	if published != "true" && published != "false" {
		return apiv1.Verdict{}, "", false, fmt.Errorf("push-remediated result has invalid published output %q", published)
	}

	data, err := rd.ArtifactBytes(*contextRef)
	if err != nil {
		return apiv1.Verdict{}, "", false, fmt.Errorf("read pr-context.json artifact: %w", err)
	}
	var context remediationContextArtifact
	if err := json.Unmarshal(data, &context); err != nil {
		return apiv1.Verdict{}, "", false, fmt.Errorf("unmarshal pr-context.json artifact: %w", err)
	}
	if context.Verdict == nil {
		return apiv1.Verdict{}, rawResponses, published == "true", nil
	}
	return *context.Verdict, rawResponses, published == "true", nil
}

func validateFindingResponses(findings []apiv1.Finding, raw string) ([]findingDisposition, error) {
	if strings.TrimSpace(raw) == "" {
		if len(findings) == 0 {
			return []findingDisposition{}, nil
		}
		return nil, fmt.Errorf("latest implement result omitted %s for %d finding(s)", findingResponsesOutput, len(findings))
	}

	var responses []findingDisposition
	if err := json.Unmarshal([]byte(raw), &responses); err != nil {
		return nil, fmt.Errorf("decode JSON array: %w", err)
	}
	if len(responses) != len(findings) {
		return nil, fmt.Errorf("contains %d response(s), want exactly %d", len(responses), len(findings))
	}

	seen := make(map[int]bool, len(responses))
	for i := range responses {
		response := &responses[i]
		response.Disposition = strings.ToLower(strings.TrimSpace(response.Disposition))
		response.Detail = strings.TrimSpace(response.Detail)
		if response.Finding < 1 || response.Finding > len(findings) {
			return nil, fmt.Errorf("response %d names finding %d outside the valid range 1..%d", i+1, response.Finding, len(findings))
		}
		if seen[response.Finding] {
			return nil, fmt.Errorf("finding %d is accounted for more than once", response.Finding)
		}
		seen[response.Finding] = true
		if response.Disposition != "addressed" && response.Disposition != "declined" {
			return nil, fmt.Errorf("finding %d disposition is %q, want addressed or declined", response.Finding, response.Disposition)
		}
		if response.Detail == "" {
			return nil, fmt.Errorf("finding %d has no detail describing what changed or why it was declined", response.Finding)
		}
	}
	sort.Slice(responses, func(i, j int) bool {
		return responses[i].Finding < responses[j].Finding
	})
	return responses, nil
}

func remediationResponseMarker(runID string) string {
	return "<!-- goobers:remediation-response:" + runID + " -->"
}

func renderRemediationResponse(runID string, result remediationResponseResult) string {
	var b strings.Builder
	b.WriteString(remediationResponseMarker(runID))
	b.WriteString("\n## Remediation response\n")
	if len(result.Findings) == 0 {
		b.WriteString("\nThis remediation cycle had no merge-review findings to account for.\n")
		return b.String()
	}
	for _, response := range result.Findings {
		finding := response.Original
		disposition := "Addressed"
		if response.Disposition == "declined" {
			disposition = "Declined"
		}
		fmt.Fprintf(&b, "\n%d. **%s** - %s\n", response.Finding, disposition, response.Detail)
		fmt.Fprintf(&b, "   > [%s", finding.Severity)
		if finding.Class != "" {
			fmt.Fprintf(&b, "/%s", finding.Class)
		}
		fmt.Fprintf(&b, "] %s", finding.Message)
		if finding.Location != "" {
			fmt.Fprintf(&b, " (%s)", finding.Location)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func reconcileRemediationResponseComment(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	prNumber int,
	runID, body string,
) error {
	author, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return fmt.Errorf("resolve remediation response author: %w", err)
	}
	id := strconv.Itoa(prNumber)
	comments, err := provider.ListComments(ctx, repo, id)
	if err != nil {
		return fmt.Errorf("list remediation response comments: %w", err)
	}
	matches := remediationResponseComments(comments, author, runID)
	if len(matches) == 0 {
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo,
			ID:         id,
			Comment:    body,
		}); err != nil {
			return fmt.Errorf("create remediation response comment: %w", err)
		}
	} else if err := provider.UpdateComment(ctx, repo, matches[0].ID, body); err != nil {
		return fmt.Errorf("update remediation response comment: %w", err)
	}

	comments, err = provider.ListComments(ctx, repo, id)
	if err != nil {
		return fmt.Errorf("relist remediation response comments: %w", err)
	}
	matches = remediationResponseComments(comments, author, runID)
	if len(matches) == 0 {
		return fmt.Errorf("remediation response comment disappeared during reconciliation")
	}
	if matches[0].Body != body {
		if err := provider.UpdateComment(ctx, repo, matches[0].ID, body); err != nil {
			return fmt.Errorf("update canonical remediation response comment: %w", err)
		}
	}
	for _, duplicate := range matches[1:] {
		if err := provider.DeleteComment(ctx, repo, duplicate.ID); err != nil {
			return fmt.Errorf("delete duplicate remediation response comment %s: %w", duplicate.ID, err)
		}
	}
	return nil
}

func remediationResponseComments(comments []providers.Comment, author, runID string) []providers.Comment {
	marker := remediationResponseMarker(runID)
	var matches []providers.Comment
	for _, comment := range comments {
		if strings.EqualFold(comment.Author, author) &&
			(comment.Body == marker || strings.HasPrefix(comment.Body, marker+"\n")) {
			matches = append(matches, comment)
		}
	}
	return matches
}
