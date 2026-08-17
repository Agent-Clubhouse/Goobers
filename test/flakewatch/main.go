// Command flakewatch scans GitHub checks and Actions job logs for test failures.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/flake"
)

const (
	reportSchema = "goobers.dev/stress/v1"
	flakeLabel   = "ci:flake"
)

var (
	testNamePattern = regexp.MustCompile(`\b(Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*)\b`)
	packagePattern  = regexp.MustCompile(`(?:^|\s)(github\.com/goobers/goobers/[A-Za-z0-9_./-]+|\./[A-Za-z0-9_./-]+)`)
	fingerprintMark = regexp.MustCompile(`<!-- goobers-flake-fingerprint:([0-9a-f]{64}) -->`)
	ledgerPackage   = regexp.MustCompile("(?m)^- \\*\\*Package:\\*\\* `([^`]+)`$")
	ledgerTest      = regexp.MustCompile("(?m)^- \\*\\*Test:\\*\\* `([^`]+)`$")
	ledgerSignature = regexp.MustCompile("(?m)^- \\*\\*Normalized signature:\\*\\* `([^`]+)`$")
	goTestRun       = regexp.MustCompile(`^=== (?:RUN|CONT)\s+(Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*)$`)
	goTestPause     = regexp.MustCompile(`^=== PAUSE\s+(Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*)$`)
	goTestFailure   = regexp.MustCompile(`^--- FAIL: (Test[A-Za-z0-9_]+(?:/[A-Za-z0-9_.-]+)*)(?: \(|$)`)
	goPackageFail   = regexp.MustCompile(`^FAIL\s+(github\.com/goobers/goobers(?:/[A-Za-z0-9_./-]+)?)\s`)
	goPackageDone   = regexp.MustCompile(`^(?:ok|\?)\s+github\.com/goobers/goobers(?:/[A-Za-z0-9_./-]+)?\s`)
	goTestTimeout   = regexp.MustCompile(`^panic: test timed out(?: after .*)?$`)
	actionTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[0-9:.+-]+Z\s+`)
)

type options struct {
	repository string
	apiURL     string
	branch     string
	output     string
	lookback   time.Duration
}

type githubClient struct {
	base       string
	repository string
	branch     string
	token      string
	http       *http.Client
}

type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

type workflowRun struct {
	ID         int64     `json:"id"`
	HeadSHA    string    `json:"head_sha"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
	HeadBranch string    `json:"head_branch"`
}

type checkRun struct {
	ID          int64     `json:"id"`
	JobID       int64     `json:"-"`
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"html_url"`
	CompletedAt time.Time `json:"completed_at"`
	Output      struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

type workflowJob struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Conclusion  string `json:"conclusion"`
	HTMLURL     string `json:"html_url"`
	CheckRunURL string `json:"check_run_url"`
}

type annotation struct {
	Path      string `json:"path"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	RawDetail string `json:"raw_details"`
}

type ledgerIssue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type issueComment struct {
	Body string `json:"body"`
}

type ledgerEntry struct {
	Issue       int
	Fingerprint string
	Package     string
	Test        string
	Signature   string
}

type source struct {
	SHA          string
	BaseSHA      string
	URL          string
	RunID        int64
	PullRequest  int
	ChangedFiles map[string]bool
	Since        time.Time
}

type failure struct {
	Fingerprint          string    `json:"fingerprint"`
	Package              string    `json:"package"`
	Test                 string    `json:"test"`
	FailureSignature     string    `json:"failure_signature"`
	FailureText          string    `json:"failure_text"`
	FailureTextTruncated bool      `json:"failure_text_truncated"`
	LastSeenRun          string    `json:"last_seen_run"`
	LastSeenAt           time.Time `json:"last_seen_at"`
	Occurrences          int       `json:"occurrences"`
	SourcePath           string    `json:"-"`
	Occurrence           string    `json:"-"`
}

type failuresReport struct {
	SchemaVersion string `json:"schema_version"`
	Run           struct {
		RunID      string    `json:"run_id"`
		RunAttempt string    `json:"run_attempt"`
		URL        string    `json:"url"`
		SHA        string    `json:"sha"`
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
	} `json:"run"`
	Failures     []failure     `json:"failures"`
	LogOmissions []logOmission `json:"log_omissions,omitempty"`
}

type scanResult struct {
	KnownDispatched int
	Novel           []failure
	LogOmissions    []logOmission
}

type failureScan struct {
	Failures     []failure
	LogOmissions []logOmission
}

type logOmission struct {
	JobID   int64  `json:"job_id"`
	JobName string `json:"job_name"`
	JobURL  string `json:"job_url,omitempty"`
	Reason  string `json:"reason"`
}

type httpStatusError struct {
	Method   string
	Endpoint string
	Status   int
	Message  string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Endpoint, e.Status, e.Message)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, http.DefaultClient, time.Now))
}

func run(
	args []string,
	stdout, stderr io.Writer,
	getenv func(string) string,
	httpClient *http.Client,
	now func() time.Time,
) int {
	opts, err := parseOptions(args, stderr, getenv)
	if err != nil {
		return 2
	}
	token := strings.TrimSpace(getenv("GITHUB_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "flakewatch: GITHUB_TOKEN is required")
		return 2
	}
	client := &githubClient{
		base:       strings.TrimRight(opts.apiURL, "/"),
		repository: opts.repository,
		branch:     opts.branch,
		token:      token,
		http:       httpClient,
	}
	started := now().UTC()
	result, err := scan(context.Background(), client, started.Add(-opts.lookback), started)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "flakewatch: %v\n", err)
		return 1
	}
	report := failuresReport{
		SchemaVersion: reportSchema,
		Failures:      result.Novel,
		LogOmissions:  result.LogOmissions,
	}
	report.Run.RunID = getenv("GITHUB_RUN_ID")
	report.Run.RunAttempt = getenv("GITHUB_RUN_ATTEMPT")
	report.Run.URL = runURL(getenv)
	report.Run.SHA = getenv("GITHUB_SHA")
	report.Run.StartedAt = started
	report.Run.FinishedAt = now().UTC()
	if err := writeReport(opts.output, report); err != nil {
		_, _ = fmt.Fprintf(stderr, "flakewatch: write report: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(
		stdout,
		"flake watch: %d known dispatched, %d novel candidate(s), %d unavailable job log(s)\n",
		result.KnownDispatched,
		len(result.Novel),
		len(result.LogOmissions),
	)
	return 0
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	flags := flag.NewFlagSet("flakewatch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := options{
		repository: getenv("GITHUB_REPOSITORY"),
		apiURL:     getenv("GITHUB_API_URL"),
		branch:     getenv("GITHUB_DEFAULT_BRANCH"),
		output:     "flake-watch-results/failures.json",
		lookback:   24 * time.Hour,
	}
	flags.StringVar(&opts.repository, "repository", opts.repository, "GitHub repository (owner/name)")
	flags.StringVar(&opts.apiURL, "api-url", opts.apiURL, "GitHub API base URL")
	flags.StringVar(&opts.branch, "branch", opts.branch, "default branch")
	flags.StringVar(&opts.output, "output", opts.output, "novel-candidate report path")
	flags.DurationVar(&opts.lookback, "lookback", opts.lookback, "default-branch run lookback")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 || opts.lookback <= 0 || strings.TrimSpace(opts.output) == "" {
		return options{}, errors.New("usage: go run ./test/flakewatch [-lookback 24h] [-output path]")
	}
	parts := strings.Split(opts.repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return options{}, fmt.Errorf("repository must be owner/name, got %q", opts.repository)
	}
	if strings.TrimSpace(opts.apiURL) == "" {
		opts.apiURL = "https://api.github.com"
	}
	if strings.TrimSpace(opts.branch) == "" {
		opts.branch = "main"
	}
	return opts, nil
}

func scan(ctx context.Context, client *githubClient, since, observed time.Time) (scanResult, error) {
	ledger, err := client.ledger(ctx)
	if err != nil {
		return scanResult{}, fmt.Errorf("load flake ledger: %w", err)
	}
	sources, err := client.sources(ctx, since)
	if err != nil {
		return scanResult{}, fmt.Errorf("load failure sources: %w", err)
	}
	result := scanResult{Novel: []failure{}}
	seen := make(map[string]bool)
	novelIndex := make(map[string]int)
	for _, failureSource := range sources {
		scanned, err := client.failures(ctx, failureSource, observed)
		if err != nil {
			return result, fmt.Errorf("scan %s: %w", failureSource.SHA, err)
		}
		result.LogOmissions = append(result.LogOmissions, scanned.LogOmissions...)
		for _, candidate := range scanned.Failures {
			key := candidate.Occurrence
			if key == "" {
				key = candidate.Fingerprint + "\x00" + failureSource.URL
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if failureSource.PullRequest != 0 && correlatedWithPR(failureSource.ChangedFiles, candidate) {
				continue
			}
			entry, known := knownFailure(ledger, candidate)
			if known {
				dispatched, err := client.wasDispatched(ctx, entry, candidate)
				if err != nil {
					return result, fmt.Errorf("check known flake handoff %s: %w", candidate.Fingerprint, err)
				}
				if dispatched {
					continue
				}
				if err := client.dispatch(ctx, entry, candidate, failureSource); err != nil {
					return result, fmt.Errorf("dispatch known flake %s: %w", candidate.Fingerprint, err)
				}
				if err := client.recordDispatch(ctx, entry, candidate); err != nil {
					return result, fmt.Errorf("record known flake handoff %s: %w", candidate.Fingerprint, err)
				}
				result.KnownDispatched++
				continue
			}
			if index, found := novelIndex[candidate.Fingerprint]; found {
				result.Novel[index].Occurrences++
				result.Novel[index].LastSeenRun = candidate.LastSeenRun
				result.Novel[index].LastSeenAt = candidate.LastSeenAt
			} else {
				novelIndex[candidate.Fingerprint] = len(result.Novel)
				result.Novel = append(result.Novel, candidate)
			}
		}
	}
	return result, nil
}

func (c *githubClient) ledger(ctx context.Context) ([]ledgerEntry, error) {
	issues, err := getAll[ledgerIssue](ctx, c, "/repos/"+c.repository+"/issues", url.Values{
		"state": {"all"}, "labels": {flakeLabel}, "per_page": {"100"},
	})
	if err != nil {
		return nil, err
	}
	entries := make([]ledgerEntry, 0, len(issues))
	for _, issue := range issues {
		fingerprint := fingerprintMark.FindStringSubmatch(issue.Body)
		if len(fingerprint) != 2 {
			continue
		}
		entry := ledgerEntry{Issue: issue.Number, Fingerprint: fingerprint[1]}
		if match := ledgerPackage.FindStringSubmatch(issue.Body); len(match) == 2 {
			entry.Package = match[1]
		}
		if match := ledgerTest.FindStringSubmatch(issue.Body); len(match) == 2 {
			entry.Test = match[1]
		}
		if match := ledgerSignature.FindStringSubmatch(issue.Body); len(match) == 2 {
			entry.Signature = match[1]
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *githubClient) sources(ctx context.Context, since time.Time) ([]source, error) {
	created := ">=" + since.UTC().Format(time.RFC3339)
	pulls, err := getAll[pullRequest](ctx, c, "/repos/"+c.repository+"/pulls", url.Values{
		"state": {"open"}, "base": {c.branch}, "per_page": {"100"},
	})
	if err != nil {
		return nil, err
	}
	sources := make([]source, 0, len(pulls)+20)
	seenRun := make(map[int64]bool)
	for _, pull := range pulls {
		files, err := c.pullFiles(ctx, pull.Number)
		if err != nil {
			return nil, fmt.Errorf("list PR #%d files: %w", pull.Number, err)
		}
		sources = append(sources, source{
			SHA: pull.Head.SHA, BaseSHA: pull.Base.SHA, URL: pull.HTMLURL,
			PullRequest: pull.Number, ChangedFiles: files, Since: since,
		})
		runs, err := c.workflowRuns(ctx, url.Values{
			"status": {"completed"}, "head_sha": {pull.Head.SHA}, "created": {created}, "per_page": {"100"},
		})
		if err != nil {
			return nil, fmt.Errorf("list PR #%d runs: %w", pull.Number, err)
		}
		for _, run := range runs {
			if run.CreatedAt.Before(since) || seenRun[run.ID] {
				continue
			}
			sources = append(sources, source{
				SHA: pull.Head.SHA, BaseSHA: pull.Base.SHA, URL: run.HTMLURL, RunID: run.ID,
				PullRequest: pull.Number, ChangedFiles: files, Since: since,
			})
			seenRun[run.ID] = true
		}
	}
	runs, err := c.workflowRuns(ctx, url.Values{
		"status": {"completed"}, "branch": {c.branch}, "created": {created}, "per_page": {"100"},
	})
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.CreatedAt.Before(since) || seenRun[run.ID] {
			continue
		}
		sources = append(sources, source{SHA: run.HeadSHA, URL: run.HTMLURL, RunID: run.ID})
		seenRun[run.ID] = true
	}
	return sources, nil
}

func (c *githubClient) workflowRuns(ctx context.Context, query url.Values) ([]workflowRun, error) {
	type page struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	return getAllWrapped(ctx, c, "/repos/"+c.repository+"/actions/runs", query, func(value page) []workflowRun {
		return value.WorkflowRuns
	})
}

func (c *githubClient) pullFiles(ctx context.Context, number int) (map[string]bool, error) {
	type pullFile struct {
		Filename string `json:"filename"`
	}
	files, err := getAll[pullFile](ctx, c, "/repos/"+c.repository+"/pulls/"+strconv.Itoa(number)+"/files", url.Values{
		"per_page": {"100"},
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(files))
	for _, file := range files {
		result[file.Filename] = true
	}
	return result, nil
}

func (c *githubClient) failures(ctx context.Context, source source, observed time.Time) (failureScan, error) {
	checks, err := c.sourceChecks(ctx, source)
	if err != nil {
		return failureScan{}, err
	}
	var result failureScan
	for _, check := range checks {
		if ignoresFlakeSignals(check.Conclusion) {
			continue
		}
		annotations, err := getAll[annotation](ctx, c, "/repos/"+c.repository+"/check-runs/"+strconv.FormatInt(check.ID, 10)+"/annotations", url.Values{
			"per_page": {"100"},
		})
		if err != nil {
			return failureScan{}, err
		}
		for _, annotation := range annotations {
			text := strings.TrimSpace(strings.Join([]string{
				annotation.Title, annotation.Message, annotation.RawDetail,
			}, "\n"))
			testMatch := testNamePattern.FindStringSubmatch(text)
			if len(testMatch) != 2 {
				continue
			}
			pkg := annotationPackage(annotation.Path, text)
			signatureText := strings.TrimSpace(strings.Join([]string{
				annotation.Message, annotation.RawDetail,
			}, "\n"))
			if signatureText == "" {
				signatureText = annotation.Title
			}
			signature := flake.NormalizeSignature(signatureText)
			fingerprint := flake.Fingerprint(pkg, testMatch[1], signature)
			result.Failures = append(result.Failures, failure{
				Fingerprint:      fingerprint,
				Package:          pkg,
				Test:             testMatch[1],
				FailureSignature: signature,
				FailureText:      text,
				LastSeenRun:      source.URL,
				LastSeenAt:       observed,
				Occurrences:      1,
				SourcePath:       annotation.Path,
				Occurrence:       occurrenceID(check.ID, pkg, testMatch[1]),
			})
		}
		if source.RunID == 0 {
			continue
		}
		log, err := c.jobLog(ctx, check.JobID)
		if err != nil {
			var statusErr *httpStatusError
			if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
				result.LogOmissions = append(result.LogOmissions, logOmission{
					JobID:   check.JobID,
					JobName: check.Name,
					JobURL:  check.HTMLURL,
					Reason:  "job log unavailable (HTTP 404)",
				})
				continue
			}
			return failureScan{}, fmt.Errorf("read job %d log: %w", check.JobID, err)
		}
		for _, candidate := range parseGoTestFailures(log, source.URL, observed) {
			candidate.Occurrence = occurrenceID(check.ID, candidate.Package, candidate.Test)
			result.Failures = append(result.Failures, candidate)
		}
	}
	return result, nil
}

func ignoresFlakeSignals(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped", "cancelled", "stale", "action_required":
		return true
	default:
		return false
	}
}

func (c *githubClient) jobLog(ctx context.Context, jobID int64) (string, error) {
	data, err := c.getBytes(ctx, "/repos/"+c.repository+"/actions/jobs/"+strconv.FormatInt(jobID, 10)+"/logs")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseGoTestFailures(log, sourceURL string, observed time.Time) []failure {
	var result []failure
	var failedTests []string
	var timeoutLines []string
	testOutput := make(map[string][]string)
	verboseTests := make(map[string]bool)
	failed := make(map[string]bool)
	activeTest := ""
	flush := func(pkg string) {
		for _, test := range failedTests {
			text := strings.TrimSpace(strings.Join(testOutput[test], "\n"))
			signature := flake.NormalizeSignature(text)
			fingerprint := flake.Fingerprint(pkg, test, signature)
			result = append(result, failure{
				Fingerprint:      fingerprint,
				Package:          pkg,
				Test:             test,
				FailureSignature: signature,
				FailureText:      text,
				LastSeenRun:      sourceURL,
				LastSeenAt:       observed,
				Occurrences:      1,
			})
		}
		if len(timeoutLines) > 0 {
			text := strings.TrimSpace(strings.Join(timeoutLines, "\n"))
			signature := flake.NormalizeSignature(text)
			result = append(result, failure{
				Fingerprint:      flake.Fingerprint(pkg, "(package)", signature),
				Package:          pkg,
				Test:             "(package)",
				FailureSignature: signature,
				FailureText:      text,
				LastSeenRun:      sourceURL,
				LastSeenAt:       observed,
				Occurrences:      1,
			})
		}
		failedTests = nil
		timeoutLines = nil
		clear(testOutput)
		clear(verboseTests)
		clear(failed)
		activeTest = ""
	}
	for _, rawLine := range strings.Split(log, "\n") {
		line := actionTimestamp.ReplaceAllString(strings.TrimSpace(rawLine), "")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "##[") {
			continue
		}
		if match := goPackageFail.FindStringSubmatch(trimmed); len(match) == 2 {
			flush(annotationPackage("", match[1]))
			continue
		}
		if goPackageDone.MatchString(trimmed) {
			failedTests = nil
			timeoutLines = nil
			clear(testOutput)
			clear(verboseTests)
			clear(failed)
			activeTest = ""
			continue
		}
		if match := goTestRun.FindStringSubmatch(trimmed); len(match) == 2 {
			activeTest = match[1]
			verboseTests[activeTest] = true
			if _, exists := testOutput[activeTest]; !exists {
				testOutput[activeTest] = nil
			}
			continue
		}
		if match := goTestPause.FindStringSubmatch(trimmed); len(match) == 2 {
			if activeTest == match[1] {
				activeTest = ""
			}
			continue
		}
		if match := goTestFailure.FindStringSubmatch(trimmed); len(match) == 2 {
			if _, exists := testOutput[match[1]]; !exists {
				testOutput[match[1]] = nil
			}
			if !failed[match[1]] {
				failedTests = append(failedTests, match[1])
				failed[match[1]] = true
			}
			if verboseTests[match[1]] {
				activeTest = ""
			} else {
				// Non-verbose `go test` prints the failure marker before its
				// diagnostic output.
				activeTest = match[1]
			}
			continue
		}
		if goTestTimeout.MatchString(trimmed) {
			timeoutLines = append(timeoutLines, line)
			activeTest = ""
			continue
		}
		if len(timeoutLines) > 0 {
			timeoutLines = append(timeoutLines, line)
		} else if activeTest != "" {
			testOutput[activeTest] = append(testOutput[activeTest], line)
		}
	}
	return result
}

func occurrenceID(checkID int64, pkg, test string) string {
	return fmt.Sprintf("check:%d:%s:%s", checkID, pkg, test)
}

func (c *githubClient) sourceChecks(ctx context.Context, source source) ([]checkRun, error) {
	if source.RunID == 0 {
		type page struct {
			CheckRuns []checkRun `json:"check_runs"`
		}
		checks, err := getAllWrapped(
			ctx,
			c,
			"/repos/"+c.repository+"/commits/"+source.SHA+"/check-runs",
			url.Values{"filter": {"latest"}, "per_page": {"100"}},
			func(value page) []checkRun { return value.CheckRuns },
		)
		if err != nil {
			return nil, err
		}
		if source.Since.IsZero() {
			return checks, nil
		}
		recent := checks[:0]
		for _, check := range checks {
			if !check.CompletedAt.Before(source.Since) {
				recent = append(recent, check)
			}
		}
		return recent, nil
	}
	type page struct {
		Jobs []workflowJob `json:"jobs"`
	}
	jobs, err := getAllWrapped(
		ctx,
		c,
		"/repos/"+c.repository+"/actions/runs/"+strconv.FormatInt(source.RunID, 10)+"/jobs",
		url.Values{"filter": {"all"}, "per_page": {"100"}},
		func(value page) []workflowJob { return value.Jobs },
	)
	if err != nil {
		return nil, err
	}
	checks := make([]checkRun, 0, len(jobs))
	for _, job := range jobs {
		checkRunURL, err := url.Parse(job.CheckRunURL)
		if err != nil {
			return nil, fmt.Errorf("parse job %d check_run_url: %w", job.ID, err)
		}
		checkRunID, err := strconv.ParseInt(path.Base(checkRunURL.Path), 10, 64)
		if err != nil || checkRunID <= 0 {
			return nil, fmt.Errorf("parse job %d check_run_url %q: invalid check-run ID", job.ID, job.CheckRunURL)
		}
		checks = append(checks, checkRun{
			ID: checkRunID, JobID: job.ID, Name: job.Name, Conclusion: job.Conclusion, HTMLURL: job.HTMLURL,
		})
	}
	return checks, nil
}

func annotationPackage(annotationPath, text string) string {
	if match := packagePattern.FindStringSubmatch(text); len(match) == 2 {
		pkg := strings.TrimSuffix(match[1], ":")
		if suffix := strings.TrimPrefix(pkg, "github.com/goobers/goobers"); suffix != pkg {
			return "." + suffix
		}
		return pkg
	}
	dir := path.Dir(strings.TrimPrefix(annotationPath, "./"))
	if dir == "." || dir == "" {
		return "."
	}
	return "./" + dir
}

func knownFailure(entries []ledgerEntry, failure failure) (ledgerEntry, bool) {
	for _, entry := range entries {
		if (entry.Fingerprint != "" && entry.Fingerprint == failure.Fingerprint) ||
			(entry.Package == failure.Package &&
				entry.Test == failure.Test &&
				entry.Signature == failure.FailureSignature) {
			return entry, true
		}
	}
	return ledgerEntry{}, false
}

func correlatedWithPR(changedFiles map[string]bool, failure failure) bool {
	if changedFiles[failure.SourcePath] {
		return true
	}
	packageDir := strings.Trim(strings.TrimPrefix(failure.Package, "./"), "/")
	if packageDir == "" || packageDir == "." {
		return false
	}
	for filename := range changedFiles {
		if strings.HasPrefix(filename, packageDir+"/") {
			return true
		}
	}
	return false
}

func (c *githubClient) dispatch(ctx context.Context, entry ledgerEntry, failure failure, source source) error {
	payload := map[string]any{
		"event_type": "flake-fixer",
		"client_payload": map[string]any{
			"fingerprint":  entry.Fingerprint,
			"occurrence":   failure.Occurrence,
			"issue":        entry.Issue,
			"source_url":   source.URL,
			"pull_request": source.PullRequest,
			"package":      failure.Package,
			"test":         failure.Test,
		},
	}
	return c.request(ctx, http.MethodPost, "/repos/"+c.repository+"/dispatches", nil, payload, nil)
}

func dispatchMarker(failure failure) string {
	digest := sha256.Sum256([]byte(failure.Occurrence))
	return fmt.Sprintf("<!-- goobers-flake-dispatch:%x -->", digest)
}

func (c *githubClient) wasDispatched(ctx context.Context, entry ledgerEntry, failure failure) (bool, error) {
	if failure.Occurrence == "" {
		return false, nil
	}
	comments, err := getAll[issueComment](
		ctx,
		c,
		"/repos/"+c.repository+"/issues/"+strconv.Itoa(entry.Issue)+"/comments",
		url.Values{"per_page": {"100"}},
	)
	if err != nil {
		return false, err
	}
	marker := dispatchMarker(failure)
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}

func (c *githubClient) recordDispatch(ctx context.Context, entry ledgerEntry, failure failure) error {
	if failure.Occurrence == "" {
		return nil
	}
	body := dispatchMarker(failure) + "\nFixer handoff recorded for `" + failure.Test + "`."
	return c.request(
		ctx,
		http.MethodPost,
		"/repos/"+c.repository+"/issues/"+strconv.Itoa(entry.Issue)+"/comments",
		nil,
		map[string]string{"body": body},
		nil,
	)
}

func getAll[T any](ctx context.Context, client *githubClient, endpoint string, query url.Values) ([]T, error) {
	return getAllWrapped(ctx, client, endpoint, query, func(value []T) []T { return value })
}

func getAllWrapped[T, P any](
	ctx context.Context,
	client *githubClient,
	endpoint string,
	query url.Values,
	items func(P) []T,
) ([]T, error) {
	var result []T
	next := endpoint
	for next != "" {
		var page P
		nextPage, err := client.getPage(ctx, next, query, &page)
		if err != nil {
			return nil, err
		}
		result = append(result, items(page)...)
		next = nextPage
		query = nil
	}
	return result, nil
}

func (c *githubClient) getPage(ctx context.Context, endpoint string, query url.Values, output any) (string, error) {
	return c.requestPage(ctx, http.MethodGet, endpoint, query, nil, output)
}

func (c *githubClient) request(ctx context.Context, method, endpoint string, query url.Values, input, output any) error {
	_, err := c.requestPage(ctx, method, endpoint, query, input, output)
	return err
}

func (c *githubClient) requestPage(
	ctx context.Context,
	method, endpoint string,
	query url.Values,
	input, output any,
) (string, error) {
	target := endpoint
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = c.base + endpoint
	}
	if len(query) != 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return "", err
	}
	baseURL, err := url.Parse(c.base)
	if err != nil {
		return "", err
	}
	if req.URL.Scheme != baseURL.Scheme || req.URL.Host != baseURL.Host {
		return "", fmt.Errorf("refuse pagination outside GitHub API origin: %s", req.URL.Redacted())
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("%s %s: status %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	if output == nil {
		return nextLink(resp.Header.Get("Link")), nil
	}
	if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
		return "", fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nextLink(resp.Header.Get("Link")), nil
}

func (c *githubClient) getBytes(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpStatusError{
			Method:   http.MethodGet,
			Endpoint: endpoint,
			Status:   resp.StatusCode,
			Message:  strings.TrimSpace(string(message)),
		}
	}
	return io.ReadAll(resp.Body)
}

func nextLink(header string) string {
	for _, link := range strings.Split(header, ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}
		for _, attribute := range parts[1:] {
			if strings.TrimSpace(attribute) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(parts[0]), "<>")
			}
		}
	}
	return ""
}

func writeReport(filename string, report failuresReport) error {
	if err := os.MkdirAll(path.Dir(filename), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, append(data, '\n'), 0o644)
}

func runURL(getenv func(string) string) string {
	server := strings.TrimRight(getenv("GITHUB_SERVER_URL"), "/")
	repository := getenv("GITHUB_REPOSITORY")
	runID := getenv("GITHUB_RUN_ID")
	if server == "" || repository == "" || runID == "" {
		return ""
	}
	return server + "/" + repository + "/actions/runs/" + runID
}
