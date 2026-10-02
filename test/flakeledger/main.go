// Command flakeledger publishes structured stress failures to GitHub issues.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/flake"
	"github.com/goobers/goobers/providers"
)

const (
	stressSchema     = "goobers.dev/stress/v1"
	flakeLabel       = "ci:flake"
	flakeLabelColor  = "D73A4A"
	flakeDescription = "Fingerprint-backed intermittent test failure"
	// approvedLabel and cloudLabel put an auto-filed flake into the cloud
	// instance's backlog as approved work. Operator ruling (2026-09-07):
	// before this, flake issues carried ci:flake alone and every goobers:*
	// label was stripped on refresh, so 54 of 56 open flakes had never been
	// triaged and none could be claimed. Filing them approved and partitioned
	// is what makes the autonomous lane able to act on them at all.
	approvedLabel       = "goobers:approved"
	approvedLabelColor  = "0E8A16"
	approvedDescription = "Maintainer-approved — eligible for curation/implementation (SEC-047)"
	cloudLabel          = "goobers:cloud"
	cloudLabelColor     = "1D76DB"
	cloudDescription    = "Claim-partition: issue belongs to the cloud (Goobernetes) instance"
	snippetLimit        = 8 * 1024
	signatureLimit      = 1024
	stateOpen           = "open"
	stateClosed         = "closed"
)

var (
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// runnerFlagEcho matches a normalized signature segment that is only the Go
	// test runner repeating one of its own flags, such as `-test.shuffle
	// <value>`. A signature made of nothing else names no failure.
	runnerFlagEcho  = regexp.MustCompile(`^-test\.[A-Za-z0-9_.]+(?:[= ]\S+)?$`)
	packageSummary  = regexp.MustCompile(`^ok\s+\S+\s+(?:\(cached\)|\d+(?:\.\d+)?s)(?:\s+coverage:\s+(?:\d+(?:\.\d+)?%\s+of statements(?:\s+in\s+.+)?|\[no statements\]))?$`)
	coverageSummary = regexp.MustCompile(`^coverage:\s+(?:\d+(?:\.\d+)?%\s+of statements(?:\s+in\s+.+)?|\[no statements\])$`)
)

type options struct {
	input      string
	repository string
	apiURL     string
}

type runMetadata struct {
	RunID      string    `json:"run_id"`
	RunAttempt string    `json:"run_attempt"`
	URL        string    `json:"url"`
	SHA        string    `json:"sha"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type failuresReport struct {
	SchemaVersion string        `json:"schema_version"`
	Run           runMetadata   `json:"run"`
	Failures      []testFailure `json:"failures"`
}

type testFailure struct {
	Fingerprint          string    `json:"fingerprint"`
	Package              string    `json:"package"`
	Test                 string    `json:"test"`
	FailureSignature     string    `json:"failure_signature"`
	FailureText          string    `json:"failure_text"`
	FailureTextTruncated bool      `json:"failure_text_truncated"`
	LastSeenRun          string    `json:"last_seen_run"`
	LastSeenAt           time.Time `json:"last_seen_at"`
	Occurrences          int       `json:"occurrences"`
	// SHA is the commit the failure was observed at, when the producer scans
	// more than one commit; it falls back to the report's run SHA.
	SHA string `json:"sha,omitempty"`

	// members is set only on a grouped build-break entry: the per-package
	// failures it stands for (#4230).
	members []testFailure
}

type ledgerProvider interface {
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

type providerFactory func(token, apiURL string) ledgerProvider

type publishResult struct {
	Created    int
	Refreshed  int
	Reopened   int
	Skipped    int
	Superseded int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, newGitHubProvider))
}

func run(
	args []string,
	stdout, stderr io.Writer,
	getenv func(string) string,
	newProvider providerFactory,
) int {
	opts, err := parseOptions(args, stderr, getenv)
	if err != nil {
		return 2
	}
	token := strings.TrimSpace(getenv("GITHUB_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "flakeledger: GITHUB_TOKEN is required")
		return 2
	}
	repository, err := parseRepository(opts.repository)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flakeledger: %v\n", err)
		return 2
	}
	report, err := loadFailures(opts.input)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flakeledger: load failures: %v\n", err)
		return 1
	}
	result, err := publish(context.Background(), newProvider(token, opts.apiURL), repository, report)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flakeledger: publish: %v\n", err)
		return 1
	}
	summary := fmt.Sprintf("flake ledger: %d created, %d refreshed", result.Created, result.Refreshed)
	if result.Reopened > 0 {
		summary += fmt.Sprintf(", %d reopened", result.Reopened)
	}
	if result.Skipped > 0 {
		summary += fmt.Sprintf(", %d skipped without a distinguishing signature", result.Skipped)
	}
	if result.Superseded > 0 {
		summary += fmt.Sprintf(", %d superseded by a grouped build break", result.Superseded)
	}
	_, _ = fmt.Fprintln(stdout, summary)
	return 0
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	flags := flag.NewFlagSet("flakeledger", flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := options{
		repository: getenv("GITHUB_REPOSITORY"),
		apiURL:     getenv("GITHUB_API_URL"),
	}
	flags.StringVar(&opts.input, "input", "stress-results/failures.json", "structured stress failures report")
	flags.StringVar(&opts.repository, "repository", opts.repository, "GitHub repository (owner/name)")
	flags.StringVar(&opts.apiURL, "api-url", opts.apiURL, "GitHub API base URL")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 || strings.TrimSpace(opts.input) == "" {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/flakeledger [-input path] [-repository owner/name]")
		return options{}, errors.New("invalid arguments")
	}
	return opts, nil
}

func newGitHubProvider(token, apiURL string) ledgerProvider {
	// Occurrence comments carry their own idempotency marker. Do not let the
	// generic provider replay an ambiguous POST before this command can
	// reconcile that marker on its next invocation.
	provider := providers.NewGitHubProvider(token, providers.WithMaxTransientRetries(0))
	if strings.TrimSpace(apiURL) != "" {
		provider.BaseURL = strings.TrimRight(apiURL, "/")
	}
	return provider
}

func parseRepository(value string) (providers.RepositoryRef, error) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return providers.RepositoryRef{}, fmt.Errorf("repository must be owner/name, got %q", value)
	}
	return providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    parts[0],
		Name:     parts[1],
	}, nil
}

func loadFailures(path string) (_ failuresReport, returnErr error) {
	file, err := os.Open(path)
	if err != nil {
		return failuresReport{}, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	var report failuresReport
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&report); err != nil {
		return failuresReport{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return failuresReport{}, err
	}
	if err := validateReport(report); err != nil {
		return failuresReport{}, err
	}
	return report, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("report contains multiple JSON values")
}

func validateReport(report failuresReport) error {
	if report.SchemaVersion != stressSchema {
		return fmt.Errorf("unsupported schema %q (want %q)", report.SchemaVersion, stressSchema)
	}
	seen := make(map[string]bool)
	for index, failure := range report.Failures {
		switch {
		case !fingerprintPattern.MatchString(failure.Fingerprint):
			return fmt.Errorf("failure %d has invalid fingerprint %q", index, failure.Fingerprint)
		case seen[failure.Fingerprint]:
			return fmt.Errorf("failure %d repeats fingerprint %q", index, failure.Fingerprint)
		case strings.TrimSpace(failure.Package) == "":
			return fmt.Errorf("failure %d has no package", index)
		case strings.TrimSpace(failure.Test) == "":
			return fmt.Errorf("failure %d has no test", index)
		case strings.TrimSpace(failure.FailureSignature) == "":
			return fmt.Errorf("failure %d has no normalized signature", index)
		case failure.Occurrences < 1:
			return fmt.Errorf("failure %d has invalid occurrence count %d", index, failure.Occurrences)
		case failure.LastSeenAt.IsZero():
			return fmt.Errorf("failure %d has no observation time", index)
		}
		seen[failure.Fingerprint] = true
	}
	return nil
}

func publish(
	ctx context.Context,
	provider ledgerProvider,
	repository providers.RepositoryRef,
	report failuresReport,
) (publishResult, error) {
	// EnsureWorkItemLabels creates only what is missing and never modifies an
	// existing label, so naming the two backlog labels here cannot disturb the
	// colour/description they already carry in a live repo — it just means a
	// fresh repo can be published into without pre-seeding them by hand.
	if _, err := provider.EnsureWorkItemLabels(ctx, repository, []providers.WorkItemLabel{
		{Name: flakeLabel, Color: flakeLabelColor, Description: flakeDescription},
		{Name: approvedLabel, Color: approvedLabelColor, Description: approvedDescription},
		{Name: cloudLabel, Color: cloudLabelColor, Description: cloudDescription},
	}); err != nil {
		return publishResult{}, fmt.Errorf("ensure backlog labels: %w", err)
	}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		Labels:     []string{flakeLabel},
	})
	if err != nil {
		return publishResult{}, fmt.Errorf("list flake issues: %w", err)
	}
	existing, err := indexIssues(items)
	if err != nil {
		return publishResult{}, err
	}

	result := publishResult{}
	failures := groupBuildBreaks(report.Run, report.Failures)
	for _, failure := range failures {
		item, found := existing[failure.Fingerprint]
		if !found {
			if !distinguishingSignature(failure.FailureSignature) {
				result.Skipped++
				continue
			}
			created, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
				Repository: repository,
				Title:      issueTitle(failure),
				Body:       issueBody(report.Run, failure, supersededIssues(failure, existing)),
				Labels:     []string{flakeLabel, approvedLabel, cloudLabel},
				RunID:      "flake-" + failure.Fingerprint,
			})
			if err != nil {
				return result, fmt.Errorf("create issue for %s: %w", failure.Fingerprint, err)
			}
			existing[failure.Fingerprint] = created
			result.Created++
			continue
		}
		marker := occurrenceMarker(report.Run, failure)
		recorded := strings.Contains(item.Body, marker)
		if !recorded {
			comments, err := provider.ListComments(ctx, repository, item.ID)
			if err != nil {
				return result, fmt.Errorf("list issue %s occurrences for %s: %w", item.ID, failure.Fingerprint, err)
			}
			for _, comment := range comments {
				if strings.Contains(comment.Body, marker) {
					recorded = true
					break
				}
			}
		}
		if recorded {
			// This run's occurrence is already on the issue. Do not touch it
			// again — in particular, do not reopen an issue that was closed
			// *after* this occurrence was recorded, which would fight the
			// operator who closed it.
			continue
		}
		// A fingerprint match against a CLOSED issue means a fixed flake came
		// back (#4612). Without reopening, the comment lands on an issue nobody
		// watches and no new issue is filed either, so closing an issue would
		// permanently retire its fingerprint.
		reopen := isClosed(item)
		update := providers.UpdateWorkItemRequest{
			Repository: repository,
			ID:         item.ID,
			Comment:    occurrenceComment(report.Run, failure, reopen),
		}
		if reopen {
			update.State = stateOpen
		}
		if _, err := provider.UpdateWorkItem(ctx, update); err != nil {
			return result, fmt.Errorf("refresh issue %s for %s: %w", item.ID, failure.Fingerprint, err)
		}
		if reopen {
			result.Reopened++
			continue
		}
		result.Refreshed++
	}
	for _, failure := range failures {
		superseded, err := supersedeMembers(ctx, provider, repository, failure, existing)
		result.Superseded += superseded
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// groupBuildBreaks collapses failures that share one root cause — the same
// commit and the same normalized compile-error signature — into a single
// grouped entry (#4230). A broken shared package fails every downstream
// package that builds it; filing per package turned one break into one issue
// per consumer. A compile error seen by only one failure, or a failure with
// no known commit, keeps its per-package identity.
func groupBuildBreaks(run runMetadata, failures []testFailure) []testFailure {
	type group struct {
		sha       string
		signature string
		members   []testFailure
	}
	groups := make(map[string]*group)
	keys := make([]string, len(failures))
	for index, failure := range failures {
		sha := strings.TrimSpace(firstNonEmpty(failure.SHA, run.SHA))
		if sha == "" {
			continue
		}
		signature, ok := flake.BuildBreakSignature(failure.FailureText)
		if !ok {
			continue
		}
		key := flake.BuildBreakFingerprint(sha, signature)
		keys[index] = key
		if groups[key] == nil {
			groups[key] = &group{sha: sha, signature: signature}
		}
		groups[key].members = append(groups[key].members, failure)
	}
	result := make([]testFailure, 0, len(failures))
	emitted := make(map[string]bool)
	for index, failure := range failures {
		key := keys[index]
		if key == "" || len(groups[key].members) < 2 {
			result = append(result, failure)
			continue
		}
		if emitted[key] {
			continue
		}
		emitted[key] = true
		result = append(result, groupedFailure(key, groups[key].sha, groups[key].signature, groups[key].members))
	}
	return result
}

func groupedFailure(fingerprint, sha, signature string, members []testFailure) testFailure {
	grouped := testFailure{
		Fingerprint:          fingerprint,
		Package:              strings.Join(affectedPackages(members), ", "),
		Test:                 "(build break)",
		FailureSignature:     signature,
		FailureText:          members[0].FailureText,
		FailureTextTruncated: members[0].FailureTextTruncated,
		LastSeenRun:          members[0].LastSeenRun,
		LastSeenAt:           members[0].LastSeenAt,
		SHA:                  sha,
		members:              members,
	}
	for _, member := range members {
		// Each member saw the same break in the same runs; the group's count
		// is the most any one of them saw, not their sum.
		grouped.Occurrences = max(grouped.Occurrences, member.Occurrences)
		if member.LastSeenAt.After(grouped.LastSeenAt) {
			grouped.LastSeenAt = member.LastSeenAt
			grouped.LastSeenRun = member.LastSeenRun
		}
	}
	return grouped
}

func affectedPackages(members []testFailure) []string {
	var packages []string
	for _, member := range members {
		pkg := singleLine(member.Package)
		if !slices.Contains(packages, pkg) {
			packages = append(packages, pkg)
		}
	}
	slices.Sort(packages)
	return packages
}

// supersededIssues lists the open per-package issues a grouped entry replaces.
func supersededIssues(failure testFailure, existing map[string]providers.WorkItem) []string {
	var ids []string
	for _, member := range failure.members {
		if item, found := existing[member.Fingerprint]; found && !isClosed(item) && !slices.Contains(ids, item.ID) {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// supersedeMembers closes every open per-package issue a grouped build break
// stands for as a duplicate of the grouped issue, so one break is tracked in
// one place.
func supersedeMembers(
	ctx context.Context,
	provider ledgerProvider,
	repository providers.RepositoryRef,
	failure testFailure,
	existing map[string]providers.WorkItem,
) (int, error) {
	if len(failure.members) == 0 {
		return 0, nil
	}
	grouped, found := existing[failure.Fingerprint]
	if !found {
		return 0, nil
	}
	closed := 0
	for _, member := range failure.members {
		item, found := existing[member.Fingerprint]
		if !found || isClosed(item) || item.ID == grouped.ID {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repository,
			ID:         item.ID,
			State:      stateClosed,
			Comment:    supersededComment(grouped.ID, failure),
		}); err != nil {
			return closed, fmt.Errorf("close issue %s as duplicate of %s: %w", item.ID, grouped.ID, err)
		}
		item.State = stateClosed
		existing[member.Fingerprint] = item
		closed++
	}
	return closed, nil
}

func supersededComment(groupedID string, failure testFailure) string {
	return strings.Join([]string{
		"## Superseded by a grouped build break",
		"",
		fmt.Sprintf("Duplicate of #%s. This failure is one package's view of a build break at commit `%s` "+
			"that failed %d package(s) with the same compile error. The grouped issue tracks every affected package, "+
			"so this one is closed as a duplicate.",
			singleLine(groupedID), singleLine(failure.SHA), len(affectedPackages(failure.members))),
		"",
		"**Normalized signature:** `" + renderedSignature(failure.FailureSignature) + "`",
	}, "\n")
}

func indexIssues(items []providers.WorkItem) (map[string]providers.WorkItem, error) {
	result := make(map[string]providers.WorkItem)
	for _, item := range items {
		for _, line := range strings.Split(item.Body, "\n") {
			const prefix = "<!-- goobers-flake-fingerprint:"
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
				continue
			}
			fingerprint := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
			if !fingerprintPattern.MatchString(fingerprint) {
				continue
			}
			if previous, duplicate := result[fingerprint]; duplicate {
				return nil, fmt.Errorf("fingerprint %s appears in issues %s and %s", fingerprint, previous.ID, item.ID)
			}
			result[fingerprint] = item
		}
	}
	return result, nil
}

// distinguishingSignature reports whether a normalized signature carries
// content that names a failure. A signature that is only runner-flag echoes
// identifies nothing, so filing an issue for it would create a fresh, useless
// issue for every run instead of one issue for one defect.
func distinguishingSignature(signature string) bool {
	if signature == flake.NoStableSignature {
		return false
	}
	for _, segment := range strings.Split(signature, "|") {
		segment = strings.TrimSpace(segment)
		if segment == "" || runnerFlagEcho.MatchString(segment) || packageSummary.MatchString(segment) || coverageSummary.MatchString(segment) {
			continue
		}
		return true
	}
	return false
}

func issueTitle(failure testFailure) string {
	if len(failure.members) > 0 {
		return truncateRunes(fmt.Sprintf("[flake] build break at %s in %d package(s): %s",
			shortSHA(failure.SHA),
			len(affectedPackages(failure.members)),
			renderedSignature(failure.FailureSignature),
		), 240)
	}
	title := fmt.Sprintf("[flake] %s %s: %s",
		singleLine(failure.Package),
		singleLine(failure.Test),
		renderedSignature(failure.FailureSignature),
	)
	return truncateRunes(title, 240)
}

func issueBody(run runMetadata, failure testFailure, superseded []string) string {
	if len(failure.members) > 0 {
		return groupedIssueBody(run, failure, superseded)
	}
	return strings.Join([]string{
		fingerprintMarker(failure.Fingerprint),
		"",
		"## Flake identity",
		"",
		"- **Fingerprint:** `" + failure.Fingerprint + "`",
		"- **Package:** `" + singleLine(failure.Package) + "`",
		"- **Test:** `" + singleLine(failure.Test) + "`",
		"- **Normalized signature:** `" + renderedSignature(failure.FailureSignature) + "`",
		"",
		"## Occurrences",
		"",
		occurrenceMarker(run, failure),
		occurrenceLine(run, failure),
		"",
		"## Latest failure",
		"",
		failureSnippet(failure),
		"",
		"This issue is filed and refreshed automatically by the trusted stress workflow, and enters the cloud instance's backlog as approved work.",
	}, "\n")
}

// groupedIssueBody describes one build break and every package it failed. It
// deliberately carries no single Package/Test identity line: the issue's
// identity is the commit plus the compile-error signature.
func groupedIssueBody(run runMetadata, failure testFailure, superseded []string) string {
	lines := []string{
		fingerprintMarker(failure.Fingerprint),
		"",
		"## Build break identity",
		"",
		"One commit broke a build that several packages depend on, so each of them failed the same way. " +
			"This issue groups those failures by commit and normalized compile-error signature instead of filing one issue per package.",
		"",
		"- **Fingerprint:** `" + failure.Fingerprint + "`",
		"- **Commit:** `" + singleLine(failure.SHA) + "`",
		"- **Normalized signature:** `" + renderedSignature(failure.FailureSignature) + "`",
		"",
	}
	lines = append(lines, affectedFailureLines(failure)...)
	if len(superseded) > 0 {
		lines = append(lines, "", "## Superseded issues", "")
		for _, id := range superseded {
			lines = append(lines, "- #"+singleLine(id)+" (closed as a duplicate of this issue)")
		}
	}
	lines = append(lines,
		"",
		"## Occurrences",
		"",
		occurrenceMarker(run, failure),
		occurrenceLine(run, failure),
		"",
		"## Latest failure",
		"",
		failureSnippet(failure),
		"",
		"This issue is filed and refreshed automatically by the trusted stress workflow, and enters the cloud instance's backlog as approved work.",
	)
	return strings.Join(lines, "\n")
}

func affectedFailureLines(failure testFailure) []string {
	lines := []string{"## Affected packages", ""}
	for _, member := range failure.members {
		lines = append(lines, "- `"+singleLine(member.Package)+"` `"+singleLine(member.Test)+"` (fingerprint `"+member.Fingerprint+"`)")
	}
	return lines
}

func shortSHA(sha string) string {
	sha = singleLine(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// isClosed reports whether a work item is in the provider's closed state. The
// ledger lists issues without a state filter (the GitHub provider defaults to
// `state: all`), so a closed issue is a normal, expected match here.
func isClosed(item providers.WorkItem) bool {
	return strings.EqualFold(strings.TrimSpace(item.State), stateClosed)
}

func occurrenceComment(run runMetadata, failure testFailure, reopened bool) string {
	heading := "## Flake recurrence"
	preamble := []string{}
	if reopened {
		// A reopen is the signal that a fix regressed, so it must not read like
		// an ordinary recurrence in the issue timeline or in notifications.
		heading = "## Flake recurrence after close — reopened"
		runID := firstNonEmpty(failure.LastSeenRun, run.RunID, "unknown run")
		preamble = []string{
			"This issue was closed, but the fingerprint recurred, so the stress workflow reopened it. " +
				"Treat it as a regression of the fix rather than a first sighting. " +
				"Reopened by run `" + singleLine(runID) + "`.",
			"",
		}
	}
	lines := []string{
		occurrenceMarker(run, failure),
		"",
		heading,
		"",
	}
	lines = append(lines, preamble...)
	lines = append(lines,
		occurrenceLine(run, failure),
		"",
		"**Normalized signature:** `"+renderedSignature(failure.FailureSignature)+"`",
		"",
	)
	if len(failure.members) > 0 {
		lines = append(lines, affectedFailureLines(failure)...)
		lines = append(lines, "")
	}
	lines = append(lines, failureSnippet(failure))
	return strings.Join(lines, "\n")
}

func occurrenceLine(run runMetadata, failure testFailure) string {
	observed := failure.LastSeenAt.UTC().Format(time.RFC3339)
	runID := firstNonEmpty(failure.LastSeenRun, run.RunID, "unknown run")
	runReference := "`" + singleLine(runID) + "`"
	if strings.TrimSpace(run.URL) != "" {
		runReference = "[stress run " + singleLine(runID) + "](" + strings.TrimSpace(run.URL) + ")"
	}
	attempt := ""
	if strings.TrimSpace(run.RunAttempt) != "" {
		attempt = ", attempt " + singleLine(run.RunAttempt)
	}
	return fmt.Sprintf("- %s — %d occurrence(s) in %s%s", observed, failure.Occurrences, runReference, attempt)
}

func failureSnippet(failure testFailure) string {
	text := truncateRunes(strings.TrimSpace(failure.FailureText), snippetLimit)
	if text == "" {
		text = "(failure emitted no text)"
	}
	if failure.FailureTextTruncated || utf8.RuneCountInString(failure.FailureText) > snippetLimit {
		text += "\n… output truncated; download the stress artifact for the complete event stream"
	}
	text = strings.ReplaceAll(text, "```", "` ` `")
	return "```text\n" + text + "\n```"
}

func fingerprintMarker(fingerprint string) string {
	return "<!-- goobers-flake-fingerprint:" + fingerprint + " -->"
}

func occurrenceMarker(run runMetadata, failure testFailure) string {
	identity := strings.Join([]string{
		failure.Fingerprint,
		firstNonEmpty(failure.LastSeenRun, run.RunID),
		run.RunAttempt,
	}, "\x00")
	return fmt.Sprintf("<!-- goobers-flake-occurrence:%x -->", sha256.Sum256([]byte(identity)))
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func renderedSignature(value string) string {
	return truncateRunes(singleLine(value), signatureLimit)
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
