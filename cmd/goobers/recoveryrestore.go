package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

const recoveryRestoreHelp = "Usage: goobers recovery-restore --record <record.json> --repository <checkout> --branch <new-branch> [instance]\n\n" +
	"Restore a retained implementation archive onto freshly fetched main from\n" +
	"the matching configured repository. The archive must be snapshot.bundle\n" +
	"beside record.json. The destination checkout and index remain unchanged;\n" +
	"the new local branch must not exist. Expired records and runtime asset\n" +
	"changes are refused. This does not push, open a PR, or abandon recovery.\n"

func runRecoveryRestore(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("recovery-restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "recovery-restore")
	recordPath := fs.String("record", "", "published recovery record")
	repository := fs.String("repository", "", "destination Git repository")
	branch := fs.String("branch", "", "new local operator branch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *recordPath == "" || *repository == "" || *branch == "" || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	layout := instance.NewLayout(root)
	if err := prepareManualRoot(layout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	registry, scrubber := journal.DefaultScrubber()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	commit, err := restoreConfiguredRecovery(ctx, layout, *recordPath, *repository, *branch, registry)
	if err != nil {
		pf(stderr, "error: %s\n", scrubber.Scrub([]byte(err.Error())))
		return 1
	}
	pf(stdout, "restored %s at %s\n", *branch, commit)
	return 0
}

func restoreConfiguredRecovery(ctx context.Context, layout instance.Layout, recordPath, destination, branch string, registry *journal.RegistryScrubber) (string, error) {
	record, err := recovery.ReadRecord(recordPath)
	if err != nil {
		return "", err
	}
	if !time.Now().Before(record.RetainUntil) {
		return "", fmt.Errorf("recovery retention deadline has expired")
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return "", err
	}
	project, err := recoveryConfiguredProject(cfg, record.RepositoryKey)
	if err != nil {
		return "", err
	}
	remoteURL, err := runner.DefaultRepoCloneURL(project)
	if err != nil {
		return "", err
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return "", err
	}
	workcopies, err := filepath.Abs(layout.WorkcopiesDir())
	if err != nil {
		return "", err
	}
	gitEnv, err := buildWorktreeGitEnv(cfg, workcopies, project, nil, nil, nil, runner.DefaultRepoCloneURL, registry, stores)
	if err != nil {
		return "", err
	}
	var environment []string
	if gitEnv != nil {
		environment, err = gitEnv(ctx, remoteURL)
		if err != nil {
			return "", err
		}
	}
	main, err := recovery.FetchCurrentMain(ctx, destination, remoteURL, recoveryAuthenticationEnvironment(environment))
	if err != nil {
		return "", err
	}
	const maxRecoveryBytes = 512 << 20
	archive := filepath.Join(filepath.Dir(recordPath), recovery.BundleFileName)
	if err := recovery.ImportSnapshotBundle(ctx, destination, archive, record, maxRecoveryBytes); err != nil {
		return "", err
	}
	return recovery.RestoreSnapshot(ctx, destination, record, main, branch, maxRecoveryBytes)
}

// Credential resolvers return a complete process environment. Only transport
// authentication belongs in recovery's explicit Git overrides; ambient Git
// repository, pager, and configuration locations must not cross that boundary.
func recoveryAuthenticationEnvironment(environment []string) []string {
	var result []string
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if key == "GIT_ASKPASS" || key == "GIT_TERMINAL_PROMPT" || key == "GOOBERS_GIT_TOKEN" || key == "GOOBERS_GIT_USERNAME" || key == "GIT_CONFIG_COUNT" || strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			result = append(result, entry)
		}
	}
	return result
}

func recoveryConfiguredProject(cfg *instance.Config, key string) (apiv1.RepoRef, error) {
	var matches []apiv1.RepoRef
	for _, repo := range cfg.Repos {
		identity := providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), URL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if identity.CanonicalKey() == key {
			matches = append(matches, apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), BaseURL: repo.BaseURL, Owner: repo.Owner, Project: repo.Project, Name: repo.Name})
		}
	}
	if len(matches) != 1 {
		return apiv1.RepoRef{}, fmt.Errorf("recovery record must match exactly one configured repository")
	}
	return matches[0], nil
}
