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
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	findingResponsesOutput           = "findingResponses"
	errorCodeFindingResponsesInvalid = "finding_responses_invalid"
	remediationResponseArtifactName  = "remediation-response.json"
)

const respondToFindingsHelp = "Usage: goobers respond-to-findings [--check] [path]\n\n" +
	"Read the claimed PR's original remediation verdict and the latest\n" +
	"implement stage's findingResponses output from this run's journal.\n" +
	"Require exactly one addressed/declined disposition with a non-empty\n" +
	"detail for every finding, post the resulting changelog to the PR, and\n" +
	"write the complete structured response to the declared result file.\n" +
	"With --check, only validate the response before publication; do not\n" +
	"require push-remediated or post to the PR.\n" +
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

// remediationResponseResult records the account exactly as validated.
// FindingCount is the original merge-review verdict's finding count, which
// also partitions Findings: entries numbered 1..FindingCount answer verdict
// findings and carry their Original, entries past it answer findings raised
// during this remediation cycle and carry none.
type remediationResponseResult struct {
	SelectedNumber string                       `json:"selectedNumber"`
	SourceRunID    string                       `json:"sourceRunId"`
	FindingCount   int                          `json:"findingCount"`
	Posted         bool                         `json:"posted"`
	Reason         string                       `json:"reason,omitempty"`
	Findings       []recordedFindingDisposition `json:"findings"`
}

func runRespondToFindings(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("respond-to-findings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "respond-to-findings")
	checkOnly := fs.Bool("check", false, "validate the finding account without requiring publication or posting")
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
	selectedNumber := 0
	if !*checkOnly {
		var ok bool
		selectedNumber, ok, err = claimedPullRequestNumber(root)
		if err != nil {
			pf(stderr, "error: resolve claimed PR: %v\n", err)
			return 1
		}
		if !ok {
			pf(stderr, "error: run holds no PR claim, so there is no remediation thread to respond to\n")
			return 1
		}
	}

	verdict, rawResponses, published, err := readRemediationResponseInputs(root, runID, !*checkOnly)
	if err != nil {
		pf(stderr, "error: read remediation response inputs from journal: %v\n", err)
		return 1
	}
	responses, err := validateFindingResponses(verdict.Findings, rawResponses)
	if err != nil {
		return failFindingResponseValidation(err, stderr)
	}
	if *checkOnly {
		if err := writeProviderStageResult(
			providerInput("resultFile", remediationResponseArtifactName),
			map[string]interface{}{},
		); err != nil {
			pf(stderr, "error: write finding-response validation result: %v\n", err)
			return 2
		}
		pf(stdout, "validated complete finding response account for %d verdict finding(s) and %d additional response(s)\n",
			len(verdict.Findings), len(responses)-len(verdict.Findings))
		return 0
	}

	result := remediationResponseResult{
		SelectedNumber: strconv.Itoa(selectedNumber),
		SourceRunID:    runID,
		FindingCount:   len(verdict.Findings),
		Findings:       make([]recordedFindingDisposition, len(responses)),
	}
	for i, response := range responses {
		recorded := recordedFindingDisposition{
			Finding:     response.Finding,
			Disposition: response.Disposition,
			Detail:      response.Detail,
		}
		// Responses past FindingCount answer in-run review findings, so they
		// have no original verdict finding to quote.
		if response.Finding <= len(verdict.Findings) {
			recorded.Original = verdict.Findings[response.Finding-1]
		}
		result.Findings[i] = recorded
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
	provider, err := remediationStageProviderWithRecorder(root, repo, token, false, sidecarMutationRecorder{kind: "pr"})
	if err != nil {
		pf(stderr, "error: construct remediation provider: %v\n", err)
		return 1
	}
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

func failFindingResponseValidation(validationErr error, stderr io.Writer) int {
	message := fmt.Sprintf("validate %s: %v", findingResponsesOutput, validationErr)
	pf(stderr, "error: %s\n", message)
	resultFile := providerInput("resultFile", remediationResponseArtifactName)
	if resultFile == "" {
		return 1
	}
	if err := writeProviderStageResult(resultFile, map[string]interface{}{
		executor.OutputErrorCode:      errorCodeFindingResponsesInvalid,
		executor.OutputErrorMessage:   message,
		executor.OutputErrorRetryable: false,
	}); err != nil {
		pf(stderr, "warning: write finding-response validation result %s: %v\n", resultFile, err)
	}
	return 1
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

// remediationStageNames resolves which journal stage names carry this run's
// finding responses and its publication result.
//
// These were hardcoded to "implement" and "push-remediated", which silently
// coupled the command to ONE workflow topology: the canonical pr-remediation
// example happens to name its agentic stage "implement". A gaggle whose
// remediation stage is named anything else ("remediate" reads more honestly,
// since the stage repasses an existing branch rather than implementing an
// issue) fails here with "no implement stage result found" — and fails LATE,
// after the agent has already reviewed the findings, written the fix, committed
// it, and passed verify. The whole cycle is discarded for a naming mismatch.
//
// The defaults preserve the previous behaviour exactly; the inputs let a
// workflow declare its own stage names.
func remediationStageNames() (implementStage, pushStage string) {
	return providerInput("implementStage", "implement"),
		providerInput("pushStage", "push-remediated")
}

func readRemediationResponseInputs(root, runID string, requirePublication bool) (apiv1.Verdict, string, bool, error) {
	implementStage, pushStage := remediationStageNames()
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		return apiv1.Verdict{}, "", false, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.Verdict{}, "", false, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}

	var contextRef *journal.Ref
	var rawResponses string
	var implementFound bool
	var pushFound bool
	var published string
	for i := range events {
		event := events[i]
		// stageArtifactName, not a hard-coded "<runID>:" prefix: a pod
		// records the same artifact without the run qualifier (#4119).
		if event.Type == journal.EventArtifactRecorded &&
			stageArtifactName(runID, event.Name) == "gather-pr-context/result" &&
			event.Ref != nil {
			ref := *event.Ref
			contextRef = &ref
		}
		if event.Type == journal.EventStageFinished && event.Stage == implementStage {
			implementFound = true
			rawResponses = ""
			if raw, ok := event.Outputs[findingResponsesOutput].(string); ok {
				rawResponses = raw
			}
		}
		if event.Type == journal.EventStageFinished && event.Stage == pushStage {
			pushFound = true
			published, _ = event.Outputs[pushRemediatedPublishedOutput].(string)
		}
	}
	if contextRef == nil {
		return apiv1.Verdict{}, "", false, upstreamArtifactMissing("gather-pr-context", remediationBriefArtifact)
	}
	if !implementFound {
		return apiv1.Verdict{}, "", false, fmt.Errorf(
			"no %q stage result found in this run's journal; set the implementStage input if the remediation stage has a different name",
			implementStage)
	}
	if requirePublication {
		if !pushFound {
			return apiv1.Verdict{}, "", false, fmt.Errorf(
				"no %q stage result found in this run's journal; set the pushStage input if the publication stage has a different name",
				pushStage)
		}
		if published != "true" && published != "false" {
			return apiv1.Verdict{}, "", false, fmt.Errorf("push-remediated result has invalid published output %q", published)
		}
	}

	data, err := rd.ArtifactBytes(*contextRef)
	if err != nil {
		return apiv1.Verdict{}, "", false, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		return apiv1.Verdict{}, "", false, fmt.Errorf("unmarshal remediation-brief.json artifact: %w", err)
	}
	if brief.Schema != apiv1.RemediationBriefVersion {
		return apiv1.Verdict{}, "", false, fmt.Errorf(
			"remediation-brief.json artifact schema is %q, want %q",
			brief.Schema, apiv1.RemediationBriefVersion,
		)
	}
	if brief.GatherPRContext.Verdict == nil {
		return apiv1.Verdict{}, rawResponses, published == "true", nil
	}
	return *brief.GatherPRContext.Verdict, rawResponses, published == "true", nil
}

// parseFindingResponses decodes the findingResponses output.
//
// The canonical form is a JSON array of {finding, disposition, detail}. That
// form is nested JSON inside a JSON string value, and a model emitting it
// through a completion envelope has to escape two levels correctly. A malformed
// completion can produce
//
//	"findingResponses":"[{\"finding\":1,...\"detail\":\"...\"}]},"summary":...
//
// -- the inner array closed, then the outer string value was never terminated.
// The whole result is discarded on a quoting error in
// the accounting, and the PR sat unremediated.
//
// So a line-oriented fallback is accepted: one finding per line, as
//
//	1: addressed: added the negative assertions
//	2: declined: out of scope for this item
//
// It carries exactly the same information with no nested quoting to get wrong.
// JSON stays first so existing workflows and the canonical examples are
// untouched; the fallback only runs when JSON decoding fails.
func parseFindingResponses(raw string) ([]findingDisposition, error) {
	trimmed := strings.TrimSpace(raw)

	var responses []findingDisposition
	jsonErr := json.Unmarshal([]byte(trimmed), &responses)
	if jsonErr == nil {
		return responses, nil
	}

	lineResponses, lineErr := parseFindingResponseLines(trimmed)
	if lineErr == nil && len(lineResponses) > 0 {
		return lineResponses, nil
	}

	// Report the JSON failure: it is the canonical form, so its error is the
	// more useful diagnostic when neither shape parses.
	return nil, fmt.Errorf("decode JSON array: %w (a line-oriented \"N: disposition: detail\" form is also accepted)", jsonErr)
}

// parseFindingResponseLines parses the line-oriented fallback form. Each
// non-empty line must be "<n>: <addressed|declined>: <detail>". Leading list
// markers ("-", "*") and a "#" before the number are tolerated, since a model
// asked for a list tends to produce one.
func parseFindingResponseLines(raw string) ([]findingDisposition, error) {
	var out []findingDisposition
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*	 ")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "#")

		numberPart, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %q is not \"<n>: <disposition>: <detail>\"", line)
		}
		number, err := strconv.Atoi(strings.TrimSpace(numberPart))
		if err != nil {
			return nil, fmt.Errorf("line %q does not start with a finding number", line)
		}
		dispositionPart, detail, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("line %q has no detail after the disposition", line)
		}
		out = append(out, findingDisposition{
			Finding:     number,
			Disposition: strings.TrimSpace(dispositionPart),
			Detail:      strings.TrimSpace(detail),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no finding response lines found")
	}
	return out, nil
}

// validateFindingResponses enforces the remediation account contract against
// the original merge-review verdict: every verdict finding needs exactly one
// addressed/declined disposition with a detail. Responses numbered past the
// verdict's finding count account for findings raised by an in-run reviewer
// repass; they are validated structurally and bind to no original finding.
// Runs whose remediation cause carries no verdict at all (failing-ci,
// sibling-overlap) have nothing to account for, so responses are optional
// there rather than required to be absent.
func validateFindingResponses(findings []apiv1.Finding, raw string) ([]findingDisposition, error) {
	if strings.TrimSpace(raw) == "" {
		if len(findings) == 0 {
			return []findingDisposition{}, nil
		}
		return nil, fmt.Errorf("latest implement result omitted %s for %d finding(s)", findingResponsesOutput, len(findings))
	}

	responses, err := parseFindingResponses(raw)
	if err != nil {
		return nil, err
	}

	seen := make(map[int]bool, len(responses))
	for i := range responses {
		response := &responses[i]
		response.Disposition = strings.ToLower(strings.TrimSpace(response.Disposition))
		response.Detail = strings.TrimSpace(response.Detail)
		if response.Finding < 1 {
			return nil, fmt.Errorf("response %d names finding %d, want a 1-based finding number", i+1, response.Finding)
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
	for i := range findings {
		if !seen[i+1] {
			return nil, fmt.Errorf(
				"%s has no response; every one of the verdict's %d finding(s) needs exactly one",
				describeVerdictFinding(i+1, findings[i]), len(findings),
			)
		}
	}
	sort.Slice(responses, func(i, j int) bool {
		return responses[i].Finding < responses[j].Finding
	})
	return responses, nil
}

func describeVerdictFinding(number int, finding apiv1.Finding) string {
	if finding.Message == "" {
		return fmt.Sprintf("verdict finding %d", number)
	}
	if finding.Location == "" {
		return fmt.Sprintf("verdict finding %d (%s)", number, finding.Message)
	}
	return fmt.Sprintf("verdict finding %d (%s at %s)", number, finding.Message, finding.Location)
}

func remediationResponseMarker(runID string) string {
	return "<!-- goobers:remediation-response:" + runID + " -->"
}

func renderRemediationResponse(runID string, result remediationResponseResult) string {
	var b strings.Builder
	b.WriteString(remediationResponseMarker(runID))
	b.WriteString("\n## Remediation response\n")
	if result.FindingCount == 0 {
		b.WriteString("\nThis remediation cycle had no merge-review findings to account for.\n")
	}
	var additional []recordedFindingDisposition
	for _, response := range result.Findings {
		if response.Finding > result.FindingCount {
			additional = append(additional, response)
			continue
		}
		finding := response.Original
		fmt.Fprintf(&b, "\n%d. **%s** - %s\n", response.Finding, dispositionLabel(response.Disposition), response.Detail)
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
	if len(additional) > 0 {
		b.WriteString("\n### Raised during this remediation cycle\n")
		for _, response := range additional {
			fmt.Fprintf(&b, "\n- **%s** - %s\n", dispositionLabel(response.Disposition), response.Detail)
		}
	}
	return b.String()
}

func dispositionLabel(disposition string) string {
	if disposition == "declined" {
		return "Declined"
	}
	return "Addressed"
}

func reconcileRemediationResponseComment(
	ctx context.Context,
	provider remediationProvider,
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
