package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/samples"
)

const (
	onboardingActionVersion       = 1
	stubSampleAction              = "stub-sample"
	stubSampleRoot                = "getting-started-task-api"
	defaultWorkTrackingTokenEnv   = "GOOBERS_GITHUB_ISSUES_TOKEN"
	stubSampleProviderTimeout     = 30 * time.Second
	stubSampleNextInstanceCommand = "goobers init --template=quickstart ./tutorial-instance"
)

const onboardingHelp = "Usage: goobers onboarding <stub-sample> [flags]\n\n" +
	"Run non-interactive, machine-readable onboarding actions. Actions never\n" +
	"prompt, write secrets, create a remote, or touch a repository that was not\n" +
	"explicitly named.\n\n" +
	"Commands:\n" +
	"  stub-sample  materialize the disposable Getting Started target\n\n" +
	"Run `goobers onboarding stub-sample -h` for action flags.\n"

const stubSampleHelp = "Usage: goobers onboarding stub-sample --destination <path> [flags]\n\n" +
	"Materialize the embedded getting-started-task-api sample at an explicitly\n" +
	"named destination. Existing matching files are skipped. A conflicting file\n" +
	"fails the complete preflight without changing the destination unless --force\n" +
	"is set; symbolic links are always refused.\n\n" +
	"With --work-tracking owner/repo, seed the catalog's missing GitHub labels and\n" +
	"issues using the token named by --token-env. If the token is unset, report\n" +
	"the issues pending and complete the local materialization without network\n" +
	"access. No remote repository is created or pushed.\n\n" +
	"Flags:\n" +
	"  --destination <path>      required sample destination\n" +
	"  --work-tracking <repo>    optional GitHub owner/repo to seed\n" +
	"  --token-env <name>        issue token environment variable (default " + defaultWorkTrackingTokenEnv + ")\n" +
	"  --force                   replace conflicting regular files\n" +
	"  --json                    emit the versioned action envelope\n\n" +
	"Exit codes: 0 = materialized, 1 = conflict/provider error, 2 = usage error.\n"

type onboardingActionResult struct {
	Action      string   `json:"action"`
	Version     int      `json:"version"`
	Created     []string `json:"created"`
	Skipped     []string `json:"skipped"`
	Path        string   `json:"path"`
	NextCommand string   `json:"nextCommand"`
}

type onboardingSeedCatalog struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Sample        onboardingSeedSample      `json:"sample"`
	Labels        []providers.WorkItemLabel `json:"labels"`
	Issues        []onboardingSeedIssue     `json:"issues"`
}

type onboardingSeedSample struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type onboardingSeedIssue struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

type onboardingSampleFile struct {
	path string
	data []byte
}

type onboardingIssueSeeder interface {
	EnsureWorkItemLabels(context.Context, providers.RepositoryRef, []providers.WorkItemLabel) (providers.EnsureWorkItemLabelsResult, error)
	ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error)
}

var newOnboardingIssueSeeder = func(token string) onboardingIssueSeeder {
	return providers.NewGitHubProvider(token)
}

func runOnboarding(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelpArg(args[0]) {
		pf(stdout, "%s", onboardingHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown onboarding command %q\n", args[0])
	}
	pf(stderr, "%s", onboardingHelp)
	return 2
}

func runOnboardingStubSample(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("onboarding stub-sample", flag.ContinueOnError)
	flags.SetOutput(stderr)
	destination := flags.String("destination", "", "sample destination")
	workTracking := flags.String("work-tracking", "", "GitHub owner/repo to seed")
	tokenEnv := flags.String("token-env", defaultWorkTrackingTokenEnv, "issue token environment variable")
	force := flags.Bool("force", false, "replace conflicting regular files")
	jsonOutput := flags.Bool("json", false, "emit the versioned action envelope")
	flags.Usage = helpUsage(stderr, "onboarding stub-sample")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*destination) == "" {
		flags.Usage()
		return 2
	}
	if !instance.ValidGuidedTokenEnvName(*tokenEnv) {
		pf(stderr, "error: --token-env must name a valid environment variable\n")
		return 2
	}

	var repository *providers.RepositoryRef
	if strings.TrimSpace(*workTracking) != "" {
		owner, name, err := parseGitHubRepo(*workTracking)
		if err != nil {
			pf(stderr, "error: --work-tracking: %v\n", err)
			return 2
		}
		repository = &providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    owner,
			Name:     name,
		}
	}

	files, catalog, err := loadOnboardingSample()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	result, err := materializeOnboardingSample(*destination, files, *force)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	switch {
	case repository == nil:
		appendPendingSeedIssues(&result, catalog, "work-tracking target not supplied")
	case os.Getenv(*tokenEnv) == "":
		appendPendingSeedIssues(&result, catalog, "credentials unavailable")
	default:
		ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
		defer cancel()
		if err := seedOnboardingIssues(ctx, newOnboardingIssueSeeder(os.Getenv(*tokenEnv)), *repository, catalog, &result); err != nil {
			pf(stderr, "error: seed %s/%s: %v\n", repository.Owner, repository.Name, err)
			return 1
		}
	}

	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			pf(stderr, "error: encode action result: %v\n", err)
			return 1
		}
		return 0
	}
	printOnboardingActionResult(stdout, result)
	return 0
}

func loadOnboardingSample() ([]onboardingSampleFile, onboardingSeedCatalog, error) {
	root, err := fs.Sub(samples.Files, stubSampleRoot)
	if err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("open embedded sample: %w", err)
	}
	var files []onboardingSampleFile
	if err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("embedded sample contains symbolic link %s", path)
		}
		data, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		files = append(files, onboardingSampleFile{path: filepath.ToSlash(path), data: data})
		return nil
	}); err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("read embedded sample: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	seedData, err := fs.ReadFile(root, "seed-issues.json")
	if err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("read embedded seed issues: %w", err)
	}
	var catalog onboardingSeedCatalog
	if err := json.Unmarshal(seedData, &catalog); err != nil {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("decode embedded seed issues: %w", err)
	}
	if catalog.SchemaVersion != 1 || catalog.Sample.ID == "" || catalog.Sample.Version == "" {
		return nil, onboardingSeedCatalog{}, fmt.Errorf("embedded seed issues have an invalid version contract")
	}
	for _, issue := range catalog.Issues {
		if issue.ID == "" || issue.Title == "" {
			return nil, onboardingSeedCatalog{}, fmt.Errorf("embedded seed issue id and title are required")
		}
	}
	return files, catalog, nil
}

func materializeOnboardingSample(
	destination string,
	files []onboardingSampleFile,
	force bool,
) (onboardingActionResult, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return onboardingActionResult{}, fmt.Errorf("resolve destination: %w", err)
	}
	result := onboardingActionResult{
		Action:      stubSampleAction,
		Version:     onboardingActionVersion,
		Created:     []string{},
		Skipped:     []string{},
		Path:        absolute,
		NextCommand: stubSampleNextInstanceCommand,
	}
	if err := validateOnboardingDestination(absolute); err != nil {
		return onboardingActionResult{}, err
	}

	write := make([]bool, len(files))
	for i, file := range files {
		if file.path == "." || file.path == "" || strings.HasPrefix(file.path, "../") {
			return onboardingActionResult{}, fmt.Errorf("embedded sample path %q is unsafe", file.path)
		}
		target := filepath.Join(absolute, filepath.FromSlash(file.path))
		if err := validateOnboardingParents(absolute, filepath.Dir(target)); err != nil {
			return onboardingActionResult{}, err
		}
		info, statErr := os.Lstat(target)
		switch {
		case errors.Is(statErr, fs.ErrNotExist):
			write[i] = true
			result.Created = append(result.Created, file.path)
		case statErr != nil:
			return onboardingActionResult{}, fmt.Errorf("inspect %s: %w", target, statErr)
		case info.Mode()&fs.ModeSymlink != 0:
			return onboardingActionResult{}, fmt.Errorf("refusing symbolic link %s", target)
		case !info.Mode().IsRegular():
			return onboardingActionResult{}, fmt.Errorf("refusing non-regular file %s", target)
		default:
			current, err := os.ReadFile(target)
			if err != nil {
				return onboardingActionResult{}, fmt.Errorf("read %s: %w", target, err)
			}
			if bytes.Equal(current, file.data) {
				result.Skipped = append(result.Skipped, file.path)
			} else if !force {
				return onboardingActionResult{}, fmt.Errorf("refusing to replace user-owned file %s without --force", target)
			} else {
				write[i] = true
				result.Created = append(result.Created, file.path)
			}
		}
	}

	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return onboardingActionResult{}, fmt.Errorf("create destination: %w", err)
	}
	for i, file := range files {
		if !write[i] {
			continue
		}
		target := filepath.Join(absolute, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return onboardingActionResult{}, fmt.Errorf("create parent for %s: %w", target, err)
		}
		if err := os.WriteFile(target, file.data, 0o644); err != nil {
			return onboardingActionResult{}, fmt.Errorf("write %s: %w", target, err)
		}
		if err := os.Chmod(target, 0o644); err != nil {
			return onboardingActionResult{}, fmt.Errorf("set mode on %s: %w", target, err)
		}
	}
	return result, nil
}

func validateOnboardingDestination(destination string) error {
	info, err := os.Lstat(destination)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect destination %s: %w", destination, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("refusing symbolic-link destination %s", destination)
	case !info.IsDir():
		return fmt.Errorf("destination %s is not a directory", destination)
	default:
		return nil
	}
}

func validateOnboardingParents(destination, parent string) error {
	relative, err := filepath.Rel(destination, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("sample path escapes destination %s", destination)
	}
	current := destination
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect sample parent %s: %w", current, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing symbolic-link parent %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("sample parent %s is not a directory", current)
		}
	}
	return nil
}

func appendPendingSeedIssues(result *onboardingActionResult, catalog onboardingSeedCatalog, reason string) {
	for _, issue := range catalog.Issues {
		result.Skipped = append(result.Skipped, fmt.Sprintf("issue:%s (pending: %s)", issue.ID, reason))
	}
}

func seedOnboardingIssues(
	ctx context.Context,
	seeder onboardingIssueSeeder,
	repository providers.RepositoryRef,
	catalog onboardingSeedCatalog,
	result *onboardingActionResult,
) error {
	labels, err := seeder.EnsureWorkItemLabels(ctx, repository, catalog.Labels)
	if err != nil {
		return err
	}
	for _, name := range labels.Created {
		result.Created = append(result.Created, "label:"+name)
	}
	for _, name := range labels.Skipped {
		result.Skipped = append(result.Skipped, "label:"+name)
	}

	existing, err := seeder.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: repository,
		State:      "all",
	})
	if err != nil {
		return err
	}
	for _, issue := range catalog.Issues {
		runID := onboardingSeedRunID(catalog, issue)
		marker := "goobers run-id: " + runID
		found := false
		for _, item := range existing {
			if strings.Contains(item.Body, marker) {
				found = true
				break
			}
		}
		if found {
			result.Skipped = append(result.Skipped, "issue:"+issue.ID)
			continue
		}
		if _, err := seeder.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
			Repository: repository,
			Title:      issue.Title,
			Body:       issue.Body,
			Labels:     append([]string(nil), issue.Labels...),
			RunID:      runID,
		}); err != nil {
			return fmt.Errorf("create issue %q: %w", issue.ID, err)
		}
		result.Created = append(result.Created, "issue:"+issue.ID)
	}
	return nil
}

func onboardingSeedRunID(catalog onboardingSeedCatalog, issue onboardingSeedIssue) string {
	return fmt.Sprintf(
		"onboarding/%s/v%d/%s@%s/%s",
		stubSampleAction,
		onboardingActionVersion,
		catalog.Sample.ID,
		catalog.Sample.Version,
		issue.ID,
	)
}

func printOnboardingActionResult(stdout io.Writer, result onboardingActionResult) {
	pf(stdout, "sample target: %s\n", result.Path)
	for _, item := range result.Created {
		pf(stdout, "  created  %s\n", item)
	}
	for _, item := range result.Skipped {
		if strings.Contains(item, "(pending:") {
			pf(stdout, "  pending  %s\n", item)
		} else {
			pf(stdout, "  skipped  %s\n", item)
		}
	}
	pf(stdout, "next: %s\n", result.NextCommand)
}
