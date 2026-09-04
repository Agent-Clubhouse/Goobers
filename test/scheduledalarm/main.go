// Command scheduledalarm files one issue when a scheduled workflow fails N
// runs in a row.
//
// A scheduled workflow that has been red for weeks looks exactly like one that
// has been red for a night: nothing announces the streak, so a real, ongoing
// signal can go unactioned indefinitely (#4222). This command turns the streak
// itself into a tracked defect, and files it exactly once — while the alarm
// issue for a workflow is open, later runs of this command leave it alone.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/providers"
)

const (
	alarmLabel       = "ci:scheduled-failure"
	alarmLabelColor  = "B60205"
	alarmDescription = "A scheduled workflow has failed on consecutive runs"
	markerPrefix     = "<!-- goobers-scheduled-streak:"
	runPageSize      = 50
	requestTimeout   = 30 * time.Second
)

// failingConclusions are the run conclusions that continue a streak. Anything
// else — including a cancelled or skipped run — ends it, because only a
// completed, failed run is evidence the workflow is broken.
var failingConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
}

type options struct {
	repository   string
	apiURL       string
	workflowsDir string
	threshold    int
}

type workflowRun struct {
	ID         int64     `json:"id"`
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	CreatedAt  time.Time `json:"created_at"`
}

type streak struct {
	Workflow string
	Name     string
	Length   int
	Runs     []workflowRun
}

type alarmProvider interface {
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
}

type runLister interface {
	ScheduledRuns(ctx context.Context, workflowFile string) ([]workflowRun, error)
}

type providerFactory func(token, apiURL string) alarmProvider

type listerFactory func(token, apiURL, repository string) runLister

type alarmResult struct {
	Streaks int
	Created int
	Known   int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, newGitHubProvider, newRunLister))
}

func run(
	args []string,
	stdout, stderr io.Writer,
	getenv func(string) string,
	newProvider providerFactory,
	newLister listerFactory,
) int {
	opts, err := parseOptions(args, stderr, getenv)
	if err != nil {
		return 2
	}
	token := strings.TrimSpace(getenv("GITHUB_TOKEN"))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "scheduledalarm: GITHUB_TOKEN is required")
		return 2
	}
	repository, err := parseRepository(opts.repository)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduledalarm: %v\n", err)
		return 2
	}
	workflows, err := scheduledWorkflows(opts.workflowsDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduledalarm: read workflows: %v\n", err)
		return 1
	}
	result, err := raise(
		context.Background(),
		newProvider(token, opts.apiURL),
		newLister(token, opts.apiURL, opts.repository),
		repository,
		workflows,
		opts.threshold,
	)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scheduledalarm: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "scheduled alarm: %d streak(s) at or over %d run(s); %d filed, %d already tracked\n",
		result.Streaks, opts.threshold, result.Created, result.Known)
	return 0
}

func parseOptions(args []string, stderr io.Writer, getenv func(string) string) (options, error) {
	flags := flag.NewFlagSet("scheduledalarm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := options{
		repository: getenv("GITHUB_REPOSITORY"),
		apiURL:     getenv("GITHUB_API_URL"),
	}
	flags.StringVar(&opts.repository, "repository", opts.repository, "GitHub repository (owner/name)")
	flags.StringVar(&opts.apiURL, "api-url", opts.apiURL, "GitHub API base URL")
	flags.StringVar(&opts.workflowsDir, "workflows", ".github/workflows", "workflow directory to scan for scheduled triggers")
	flags.IntVar(&opts.threshold, "threshold", 3, "consecutive failed scheduled runs that raise the alarm")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 || strings.TrimSpace(opts.workflowsDir) == "" || opts.threshold < 2 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/scheduledalarm [-repository owner/name] [-workflows dir] [-threshold n>=2]")
		return options{}, errors.New("invalid arguments")
	}
	return opts, nil
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

// scheduledWorkflows returns the file names of every workflow with a schedule
// trigger, so the alarm covers whatever the repository schedules rather than a
// hand-maintained list that a new nightly job would silently miss.
func scheduledWorkflows(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var scheduled []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var workflow struct {
			On map[string]yaml.Node `yaml:"on"`
		}
		if err := yaml.Unmarshal(raw, &workflow); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if _, ok := workflow.On["schedule"]; ok {
			scheduled = append(scheduled, name)
		}
	}
	sort.Strings(scheduled)
	if len(scheduled) == 0 {
		return nil, fmt.Errorf("no scheduled workflows found in %s", dir)
	}
	return scheduled, nil
}

func raise(
	ctx context.Context,
	provider alarmProvider,
	lister runLister,
	repository providers.RepositoryRef,
	workflows []string,
	threshold int,
) (alarmResult, error) {
	var streaks []streak
	for _, workflow := range workflows {
		runs, err := lister.ScheduledRuns(ctx, workflow)
		if err != nil {
			return alarmResult{}, fmt.Errorf("list scheduled runs for %s: %w", workflow, err)
		}
		if found, ok := consecutiveFailures(workflow, runs, threshold); ok {
			streaks = append(streaks, found)
		}
	}
	result := alarmResult{Streaks: len(streaks)}
	if len(streaks) == 0 {
		return result, nil
	}

	if _, err := provider.EnsureWorkItemLabels(ctx, repository, []providers.WorkItemLabel{{
		Name:        alarmLabel,
		Color:       alarmLabelColor,
		Description: alarmDescription,
	}}); err != nil {
		return result, fmt.Errorf("ensure %s label: %w", alarmLabel, err)
	}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		Labels:     []string{alarmLabel},
		State:      "open",
	})
	if err != nil {
		return result, fmt.Errorf("list open alarm issues: %w", err)
	}
	tracked := trackedWorkflows(items)

	for _, found := range streaks {
		if tracked[found.Workflow] {
			result.Known++
			continue
		}
		if _, err := provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: repository,
			Title:      issueTitle(found),
			Body:       issueBody(found, threshold),
			Labels:     []string{alarmLabel},
			RunID:      "scheduled-streak-" + found.Workflow,
		}); err != nil {
			return result, fmt.Errorf("file alarm for %s: %w", found.Workflow, err)
		}
		tracked[found.Workflow] = true
		result.Created++
	}
	return result, nil
}

// consecutiveFailures counts the failing runs at the head of the run list and
// reports whether they reach the threshold.
func consecutiveFailures(workflow string, runs []workflowRun, threshold int) (streak, bool) {
	ordered := append([]workflowRun(nil), runs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].CreatedAt.After(ordered[j].CreatedAt)
	})
	var failing []workflowRun
	for _, candidate := range ordered {
		if !failingConclusions[candidate.Conclusion] {
			break
		}
		failing = append(failing, candidate)
	}
	if len(failing) < threshold {
		return streak{}, false
	}
	return streak{
		Workflow: workflow,
		Name:     strings.TrimSuffix(strings.TrimSuffix(workflow, ".yaml"), ".yml"),
		Length:   len(failing),
		Runs:     failing,
	}, true
}

func trackedWorkflows(items []providers.WorkItem) map[string]bool {
	tracked := make(map[string]bool, len(items))
	for _, item := range items {
		if strings.EqualFold(item.State, "closed") {
			continue
		}
		for _, line := range strings.Split(item.Body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, markerPrefix) || !strings.HasSuffix(line, " -->") {
				continue
			}
			workflow := strings.TrimSuffix(strings.TrimPrefix(line, markerPrefix), " -->")
			if workflow != "" {
				tracked[workflow] = true
			}
		}
	}
	return tracked
}

func issueTitle(found streak) string {
	return fmt.Sprintf("[ci] scheduled workflow %s has failed %d runs in a row", found.Name, found.Length)
}

func issueBody(found streak, threshold int) string {
	lines := []string{
		markerPrefix + found.Workflow + " -->",
		"",
		"## Streak",
		"",
		fmt.Sprintf("- **Workflow:** `%s`", found.Workflow),
		fmt.Sprintf("- **Consecutive failed scheduled runs:** %d (alarm threshold %d)", found.Length, threshold),
		"",
		"## Failed runs",
		"",
	}
	for _, failed := range found.Runs {
		reference := strconv.FormatInt(failed.ID, 10)
		if strings.TrimSpace(failed.HTMLURL) != "" {
			reference = "[" + reference + "](" + strings.TrimSpace(failed.HTMLURL) + ")"
		}
		lines = append(lines, fmt.Sprintf("- %s — run %s (`%s`)",
			failed.CreatedAt.UTC().Format(time.RFC3339), reference, failed.Conclusion))
	}
	return strings.Join(append(lines,
		"",
		"A scheduled workflow failing this many runs in a row is a standing signal, not a flake.",
		"The alarm files one issue per workflow and does not re-file while that issue is open,",
		"so close it once the workflow is green again.",
	), "\n")
}

func newGitHubProvider(token, apiURL string) alarmProvider {
	provider := providers.NewGitHubProvider(token)
	if strings.TrimSpace(apiURL) != "" {
		provider.BaseURL = strings.TrimRight(apiURL, "/")
	}
	return provider
}

type githubRunLister struct {
	base       string
	repository string
	token      string
	client     *http.Client
}

func newRunLister(token, apiURL, repository string) runLister {
	base := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	return githubRunLister{
		base:       base,
		repository: strings.TrimSpace(repository),
		token:      token,
		client:     &http.Client{Timeout: requestTimeout},
	}
}

func (l githubRunLister) ScheduledRuns(ctx context.Context, workflowFile string) (_ []workflowRun, returnErr error) {
	endpoint := l.base + "/repos/" + l.repository + "/actions/workflows/" +
		url.PathEscape(workflowFile) + "/runs?" + url.Values{
		"event":    {"schedule"},
		"status":   {"completed"},
		"per_page": {strconv.Itoa(runPageSize)},
	}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+l.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := l.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, response.Body.Close())
	}()
	if response.StatusCode == http.StatusNotFound {
		// A workflow file that has never run has no runs endpoint yet.
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: unexpected status %s", endpoint, response.Status)
	}
	var payload struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.WorkflowRuns, nil
}
