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
	"github.com/goobers/goobers/internal/journalclient"
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
// findings, entries past it answer findings an in-run reviewer raised during
// this remediation cycle. Every entry carries the Original finding it answers.
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

	verdict, inRunFindings, rawResponses, published, err := readRemediationResponseInputs(root, runID, !*checkOnly)
	if err != nil {
		pf(stderr, "error: read remediation response inputs from journal: %v\n", err)
		return 1
	}
	responses, err := validateFindingResponses(verdict.Findings, inRunFindings, rawResponses)
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
		// Responses past FindingCount answer in-run review findings; bind each
		// to the captured reviewer finding it names (#2748).
		if response.Finding <= len(verdict.Findings) {
			recorded.Original = verdict.Findings[response.Finding-1]
		} else {
			recorded.Original = inRunFindings[response.Finding-len(verdict.Findings)-1]
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
	brief, err := readLatestRemediationBrief(root, runID)
	if err != nil {
		pf(stderr, "error: read remediation brief: %v\n", err)
		return 1
	}
	comment := withFeedbackAck(renderRemediationResponse(runID, result), remediationFeedbackAck(brief))

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
	channel, err := newRemediationResponseChannel(root, repo, token)
	if err != nil {
		pf(stderr, "error: construct remediation provider: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	if err := reconcileRemediationResponseComment(ctx, channel, selectedNumber, runID, comment); err != nil {
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

// readRemediationResponseInputs returns the original merge-review verdict, the
// findings in-run reviewer gates raised before the latest implement result
// (#2748), the implementer's raw findingResponses, and whether the remediated
// branch was published.
func readRemediationResponseInputs(root, runID string, requirePublication bool) (apiv1.Verdict, []apiv1.Finding, string, bool, error) {
	implementStage, pushStage := remediationStageNames()
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		return apiv1.Verdict{}, nil, "", false, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.Verdict{}, nil, "", false, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}

	var contextRef *journal.Ref
	var rawResponses string
	var implementFound bool
	var pushFound bool
	var published string
	// reviewRefs holds the runner-recorded verdict artifact of the latest
	// agentic gate evaluated so far (the reviewer feedback a repass hands
	// implement); inRunRefs snapshots it at the latest implement result, so
	// responses are bounded by, and numbered against, the reviewer findings
	// that attempt was actually answering (#2748).
	var reviewRefs, inRunRefs []journal.Ref
	for i := range events {
		event := events[i]
		if event.Type == journal.EventGateEvaluated && event.Ref != nil {
			reviewRefs = []journal.Ref{*event.Ref}
		}
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
			inRunRefs = append([]journal.Ref(nil), reviewRefs...)
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
		return apiv1.Verdict{}, nil, "", false, upstreamArtifactMissing("gather-pr-context", remediationBriefArtifact)
	}
	if !implementFound {
		return apiv1.Verdict{}, nil, "", false, fmt.Errorf(
			"no %q stage result found in this run's journal; set the implementStage input if the remediation stage has a different name",
			implementStage)
	}
	if requirePublication {
		if !pushFound {
			return apiv1.Verdict{}, nil, "", false, fmt.Errorf(
				"no %q stage result found in this run's journal; set the pushStage input if the publication stage has a different name",
				pushStage)
		}
		if published != "true" && published != "false" {
			return apiv1.Verdict{}, nil, "", false, fmt.Errorf("push-remediated result has invalid published output %q", published)
		}
	}

	verdict, err := remediationBriefVerdict(rd, *contextRef)
	if err != nil {
		return apiv1.Verdict{}, nil, "", false, err
	}
	inRunFindings, err := inRunReviewFindings(rd, inRunRefs)
	if err != nil {
		return apiv1.Verdict{}, nil, "", false, err
	}
	return verdict, inRunFindings, rawResponses, published == "true", nil
}

// remediationBriefVerdict decodes the gather-pr-context brief and returns its
// original merge-review verdict (zero when the remediation cause carries none).
func remediationBriefVerdict(rd journalclient.Reader, ref journal.Ref) (apiv1.Verdict, error) {
	data, err := rd.ArtifactBytes(ref)
	if err != nil {
		return apiv1.Verdict{}, upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		return apiv1.Verdict{}, fmt.Errorf("unmarshal remediation-brief.json artifact: %w", err)
	}
	// Any readable wire version: a run whose gather-pr-context wrote an older
	// brief before this binary deployed must still be able to respond.
	if !apiv1.SupportedRemediationBriefVersion(brief.Schema) {
		return apiv1.Verdict{}, fmt.Errorf(
			"remediation-brief.json artifact schema is %q, want one of %s",
			brief.Schema, strings.Join(apiv1.SupportedRemediationBriefVersions(), ", "),
		)
	}
	if brief.GatherPRContext.Verdict == nil {
		return apiv1.Verdict{}, nil
	}
	return *brief.GatherPRContext.Verdict, nil
}

// inRunReviewFindings loads the findings from the given gate verdict
// artifacts in journal order. They are the ground truth for responses
// numbered past the original verdict: response N+k answers the k-th finding.
func inRunReviewFindings(rd journalclient.Reader, refs []journal.Ref) ([]apiv1.Finding, error) {
	var findings []apiv1.Finding
	for _, ref := range refs {
		data, err := rd.ArtifactBytes(ref)
		if err != nil {
			return nil, fmt.Errorf("read in-run review verdict artifact: %w", err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return nil, fmt.Errorf("unmarshal in-run review verdict artifact: %w", err)
		}
		findings = append(findings, verdict.Findings...)
	}
	return findings, nil
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
// verdict's finding count account for findings an in-run reviewer raised;
// they are optional, but each must name one of the inRun findings captured
// from this run's journal (numbered len(findings)+1 onward), so a producer
// cannot claim to have addressed a finding no reviewer raised (#2748).
// Runs whose remediation cause carries no verdict at all (failing-ci,
// sibling-overlap) have no verdict findings to account for, so responses are
// optional there rather than required to be absent.
func validateFindingResponses(findings, inRun []apiv1.Finding, raw string) ([]findingDisposition, error) {
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
		if limit := len(findings) + len(inRun); response.Finding > limit {
			return nil, fmt.Errorf(
				"response %d names finding %d, but only %d verdict finding(s) and %d in-run reviewer finding(s) were raised; "+
					"respond only to findings a reviewer raised (\"[]\" when there are none)",
				i+1, response.Finding, len(findings), len(inRun))
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
		fmt.Fprintf(&b, "\n%d. **%s** - %s\n", response.Finding, dispositionLabel(response.Disposition), response.Detail)
		writeFindingQuote(&b, response.Original)
	}
	if len(additional) > 0 {
		b.WriteString("\n### Raised during this remediation cycle\n")
		for _, response := range additional {
			fmt.Fprintf(&b, "\n- **%s** - %s\n", dispositionLabel(response.Disposition), response.Detail)
			writeFindingQuote(&b, response.Original)
		}
	}
	return b.String()
}

func writeFindingQuote(b *strings.Builder, finding apiv1.Finding) {
	fmt.Fprintf(b, "   > [%s", finding.Severity)
	if finding.Class != "" {
		fmt.Fprintf(b, "/%s", finding.Class)
	}
	fmt.Fprintf(b, "] %s", finding.Message)
	if finding.Location != "" {
		fmt.Fprintf(b, " (%s)", finding.Location)
	}
	b.WriteByte('\n')
}

func dispositionLabel(disposition string) string {
	if disposition == "declined" {
		return "Declined"
	}
	return "Addressed"
}

// remediationResponseChannel is where respond-to-findings keeps its one
// run-scoped comment on a pull request: the PR conversation (issue comments)
// on GitHub and Gitea, a pull-request thread on Azure DevOps.
type remediationResponseChannel interface {
	// authoredBySelf resolves the stage's own identity and reports whether a
	// comment was written by it.
	authoredBySelf(ctx context.Context) (func(providers.Comment) bool, error)
	list(ctx context.Context, pullID string) ([]providers.Comment, error)
	create(ctx context.Context, pullID, body string) error
	update(ctx context.Context, commentID, body string) error
	remove(ctx context.Context, commentID string) error
}

// newRemediationResponseChannel builds the channel from the stage's declared
// github:issues:write credential, the capability respond-to-findings has
// always declared for its pull-request comment. GitHub and Gitea use the broad
// remediation factory as before; Azure DevOps, whose *ADOProvider does not
// implement it, builds the narrow thread surface through
// remediationStageSurface. Both record their mutations as kind "pr".
func newRemediationResponseChannel(root string, repo providers.RepositoryRef, token string) (remediationResponseChannel, error) {
	recorder := sidecarMutationRecorder{kind: "pr"}
	if repo.Provider == providers.ProviderADO {
		provider, err := remediationStageSurface[adoRemediationResponseThreads](root, repo, token,
			withStageProviderCapability(capability.GitHubIssuesWrite), withStageProviderMutationRecorder(recorder))
		if err != nil {
			return nil, err
		}
		return threadRemediationResponseChannel{provider: provider, repo: repo}, nil
	}
	provider, err := remediationStageProviderWithRecorder(root, repo, token, false, recorder)
	if err != nil {
		return nil, err
	}
	return issueCommentRemediationResponseChannel{provider: provider, repo: repo}, nil
}

type issueCommentRemediationResponseChannel struct {
	provider remediationProvider
	repo     providers.RepositoryRef
}

func (c issueCommentRemediationResponseChannel) authoredBySelf(ctx context.Context) (func(providers.Comment) bool, error) {
	author, err := c.provider.AuthenticatedLogin(ctx)
	if err != nil {
		return nil, err
	}
	return func(comment providers.Comment) bool { return strings.EqualFold(comment.Author, author) }, nil
}

func (c issueCommentRemediationResponseChannel) list(ctx context.Context, pullID string) ([]providers.Comment, error) {
	return c.provider.ListComments(ctx, c.repo, pullID)
}

func (c issueCommentRemediationResponseChannel) create(ctx context.Context, pullID, body string) error {
	_, err := c.provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: c.repo, ID: pullID, Comment: body})
	return err
}

func (c issueCommentRemediationResponseChannel) update(ctx context.Context, commentID, body string) error {
	return c.provider.UpdateComment(ctx, c.repo, commentID, body)
}

func (c issueCommentRemediationResponseChannel) remove(ctx context.Context, commentID string) error {
	return c.provider.DeleteComment(ctx, c.repo, commentID)
}

// adoRemediationResponseThreads is the Azure DevOps pull-request thread
// surface respond-to-findings needs. Its comment is posted as a closed
// (informational) thread, so it never trips a comment-resolution policy.
type adoRemediationResponseThreads interface {
	adoIdentityReader
	ListPullRequestThreadComments(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]providers.Comment, error)
	PostPullRequestThreadComment(ctx context.Context, repo providers.RepositoryRef, pullID, body string) (providers.Comment, error)
	UpdatePullRequestThreadComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	DeletePullRequestThreadComment(ctx context.Context, repo providers.RepositoryRef, commentID string) error
}

type threadRemediationResponseChannel struct {
	provider adoRemediationResponseThreads
	repo     providers.RepositoryRef
}

// authoredBySelf matches by identity GUID (ADO-N5): display names are not
// unique on Azure DevOps.
func (c threadRemediationResponseChannel) authoredBySelf(ctx context.Context) (func(providers.Comment) bool, error) {
	self, err := c.provider.AuthenticatedIdentity(ctx)
	if err != nil {
		return nil, err
	}
	return func(comment providers.Comment) bool { return adoCommentAuthoredBy(comment, self) }, nil
}

func (c threadRemediationResponseChannel) list(ctx context.Context, pullID string) ([]providers.Comment, error) {
	return c.provider.ListPullRequestThreadComments(ctx, c.repo, pullID)
}

func (c threadRemediationResponseChannel) create(ctx context.Context, pullID, body string) error {
	_, err := c.provider.PostPullRequestThreadComment(ctx, c.repo, pullID, body)
	return err
}

func (c threadRemediationResponseChannel) update(ctx context.Context, commentID, body string) error {
	return c.provider.UpdatePullRequestThreadComment(ctx, c.repo, commentID, body)
}

func (c threadRemediationResponseChannel) remove(ctx context.Context, commentID string) error {
	return c.provider.DeletePullRequestThreadComment(ctx, c.repo, commentID)
}

func reconcileRemediationResponseComment(
	ctx context.Context,
	channel remediationResponseChannel,
	prNumber int,
	runID, body string,
) error {
	authoredBySelf, err := channel.authoredBySelf(ctx)
	if err != nil {
		return fmt.Errorf("resolve remediation response author: %w", err)
	}
	id := strconv.Itoa(prNumber)
	return reconcileCanonicalProviderComment(body, canonicalProviderCommentSpec{
		noun: "remediation response",
		list: func() ([]providers.Comment, error) {
			return channel.list(ctx, id)
		},
		create: func(body string) error {
			return channel.create(ctx, id, body)
		},
		update: func(commentID, body string) error {
			return channel.update(ctx, commentID, body)
		},
		remove: func(commentID string) error {
			return channel.remove(ctx, commentID)
		},
		match: func(comments []providers.Comment) []providers.Comment {
			return remediationResponseComments(comments, authoredBySelf, runID)
		},
	})
}

func remediationResponseComments(comments []providers.Comment, authoredBySelf func(providers.Comment) bool, runID string) []providers.Comment {
	marker := remediationResponseMarker(runID)
	var matches []providers.Comment
	for _, comment := range comments {
		if authoredBySelf(comment) &&
			(comment.Body == marker || strings.HasPrefix(comment.Body, marker+"\n")) {
			matches = append(matches, comment)
		}
	}
	return matches
}
