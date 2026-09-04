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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/providers"
)

const (
	stressSchema     = "goobers.dev/stress/v1"
	flakeLabel       = "ci:flake"
	flakeLabelColor  = "D73A4A"
	flakeDescription = "Fingerprint-backed intermittent test failure"
	snippetLimit     = 8 * 1024
	signatureLimit   = 1024
)

var (
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// runnerFlagEcho matches a normalized signature segment that is only the Go
	// test runner repeating one of its own flags, such as `-test.shuffle
	// <value>`. A signature made of nothing else names no failure.
	runnerFlagEcho = regexp.MustCompile(`^-test\.[A-Za-z0-9_.]+(?:[= ]\S+)?$`)
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
	Created   int
	Refreshed int
	Skipped   int
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
	if result.Skipped > 0 {
		summary += fmt.Sprintf(", %d skipped without a distinguishing signature", result.Skipped)
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
	if _, err := provider.EnsureWorkItemLabels(ctx, repository, []providers.WorkItemLabel{{
		Name:        flakeLabel,
		Color:       flakeLabelColor,
		Description: flakeDescription,
	}}); err != nil {
		return publishResult{}, fmt.Errorf("ensure %s label: %w", flakeLabel, err)
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
	for _, failure := range report.Failures {
		item, found := existing[failure.Fingerprint]
		if !found {
			if !distinguishingSignature(failure.FailureSignature) {
				result.Skipped++
				continue
			}
			created, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
				Repository: repository,
				Title:      issueTitle(failure),
				Body:       issueBody(report.Run, failure),
				Labels:     []string{flakeLabel},
				RunID:      "flake-" + failure.Fingerprint,
			})
			if err != nil {
				return result, fmt.Errorf("create issue for %s: %w", failure.Fingerprint, err)
			}
			existing[failure.Fingerprint] = created
			result.Created++
			continue
		}
		removeLabels := workflowLabels(item.Labels)
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
		comment := ""
		if !recorded {
			comment = occurrenceComment(report.Run, failure)
		}
		if comment == "" && len(removeLabels) == 0 {
			continue
		}
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   repository,
			ID:           item.ID,
			RemoveLabels: removeLabels,
			Comment:      comment,
		}); err != nil {
			return result, fmt.Errorf("refresh issue %s for %s: %w", item.ID, failure.Fingerprint, err)
		}
		result.Refreshed++
	}
	return result, nil
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
	for _, segment := range strings.Split(signature, "|") {
		segment = strings.TrimSpace(segment)
		if segment == "" || runnerFlagEcho.MatchString(segment) {
			continue
		}
		return true
	}
	return false
}

func workflowLabels(labels []string) []string {
	var result []string
	for _, label := range labels {
		normalized := strings.ToLower(label)
		if strings.HasPrefix(normalized, "goobers:") || strings.HasPrefix(normalized, "goobers/status:") {
			result = append(result, label)
		}
	}
	sort.Strings(result)
	return result
}

func issueTitle(failure testFailure) string {
	title := fmt.Sprintf("[flake] %s %s: %s",
		singleLine(failure.Package),
		singleLine(failure.Test),
		renderedSignature(failure.FailureSignature),
	)
	return truncateRunes(title, 240)
}

func issueBody(run runMetadata, failure testFailure) string {
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
		"This issue is maintained by the trusted stress workflow. It intentionally has no milestone or Goobers workflow labels.",
	}, "\n")
}

func occurrenceComment(run runMetadata, failure testFailure) string {
	return strings.Join([]string{
		occurrenceMarker(run, failure),
		"",
		"## Flake recurrence",
		"",
		occurrenceLine(run, failure),
		"",
		"**Normalized signature:** `" + renderedSignature(failure.FailureSignature) + "`",
		"",
		failureSnippet(failure),
	}, "\n")
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
