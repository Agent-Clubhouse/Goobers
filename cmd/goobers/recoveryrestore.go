package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

const recoveryRestoreHelp = "Usage: goobers recovery-restore --record <record.json> --repository <checkout> --branch <new-branch> [instance]\n\n" +
	"Or select by --issue <id> --repository-key <canonical-key> instead of --record.\n\n" +
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
	issueID := fs.String("issue", "", "issue with retained implementation")
	repositoryKey := fs.String("repository-key", "", "provider-complete canonical repository key")
	repository := fs.String("repository", "", "destination Git repository")
	branch := fs.String("branch", "", "new local operator branch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	explicitRecord := *recordPath != "" && *issueID == "" && *repositoryKey == ""
	issueSelection := *recordPath == "" && *issueID != "" && *repositoryKey != ""
	if (!explicitRecord && !issueSelection) || *repository == "" || *branch == "" || fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := providerStageRoot(fs.Arg(0))
	layout := instance.NewLayout(root)
	if claimsPlaneSelected() {
		if !issueSelection {
			pf(stderr, "error: recovery over the claims plane requires issue selection\n")
			return 1
		}
	} else {
		if err := prepareManualRoot(layout, stderr); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}
	registry, scrubber := journal.DefaultScrubber()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var commit string
	var err error
	if issueSelection {
		commit, err = restoreIssueRecovery(ctx, layout, *repositoryKey, *issueID, *repository, *branch, registry)
	} else {
		commit, err = restoreConfiguredRecovery(ctx, layout, *recordPath, *repository, *branch, registry)
	}
	if err != nil {
		pf(stderr, "error: %s\n", scrubber.Scrub([]byte(err.Error())))
		return 1
	}
	pf(stdout, "restored %s at %s\n", *branch, commit)
	return 0
}

func restoreIssueRecovery(ctx context.Context, layout instance.Layout, key, issue, destination, branch string, registry *journal.RegistryScrubber) (string, error) {
	if claimsPlaneSelected() {
		return restoreDownloadedRecovery(ctx, layout, key, issue, destination, branch, registry)
	}
	selected, err := selectIssueRecovery(ctx, layout, key, issue, time.Now().UTC())
	if err != nil {
		return "", err
	}
	dir, err := runDirFor(layout, selected.Record.RunID)
	if err != nil {
		return "", err
	}
	var commit string
	entered, err := journal.WithIdleRunReader(ctx, dir, func(reader *journal.Reader) error {
		identity, err := reader.Identity()
		if err != nil {
			return err
		}
		if identity.RunID != selected.Record.RunID {
			return fmt.Errorf("selected recovery run identity changed")
		}
		phase, err := reader.PhaseBounded(ctx)
		if err != nil {
			return err
		}
		if !terminalRunPhase(phase) {
			return fmt.Errorf("selected recovery run is no longer terminal")
		}
		current, err := recovery.ReadRetainedRecord(selected.RecordPath)
		if err != nil {
			return err
		}
		if current != selected.Record {
			return recovery.ErrRecordConflict
		}
		commit, err = restoreConfiguredRecovery(ctx, layout, selected.RecordPath, destination, branch, registry)
		return err
	})
	if err == nil && !entered {
		err = fmt.Errorf("selected recovery run is busy; retry when its writer is idle")
	}
	return commit, err
}

func restoreDownloadedRecovery(ctx context.Context, layout instance.Layout, key, issue, destination, branch string, registry *journal.RegistryScrubber) (string, error) {
	source := recovery.HTTPArchiveSource{
		BaseURL: os.Getenv(claimsclient.EnvEndpoint),
		Token:   os.Getenv(claimsclient.EnvToken),
		RunID:   os.Getenv(claimsclient.EnvRunID),
	}
	var commit string
	err := source.WithArchive(ctx, key, issue, func(_ recovery.Record, archive string) error {
		var err error
		commit, err = restoreConfiguredRecovery(ctx, layout, filepath.Join(filepath.Dir(archive), recovery.RecordFileName), destination, branch, registry)
		return err
	})
	return commit, err
}

func restoreConfiguredRecovery(ctx context.Context, layout instance.Layout, recordPath, destination, branch string, registry *journal.RegistryScrubber) (string, error) {
	record, err := recovery.ReadRetainedRecord(recordPath)
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
	environment, err := recoveryRestoreGitEnvironment(ctx, layout, cfg, project, remoteURL, registry)
	if err != nil {
		return "", err
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

func recoveryRestoreGitEnvironment(ctx context.Context, layout instance.Layout, cfg *instance.Config, project apiv1.RepoRef, remoteURL string, registry *journal.RegistryScrubber) ([]string, error) {
	// Pods receive a resolved repository capability, not the daemon's app
	// private key/token cache. Scope that credential to the configured URL.
	if claimsPlaneSelected() && (project.Provider == apiv1.ProviderGitHub || project.Provider == apiv1.ProviderGitea) {
		token, err := providerToken(capability.RepoPush)
		if err != nil {
			return nil, err
		}
		if project.Provider == apiv1.ProviderGitea {
			return providers.GiteaGitAuthEnvironment(token, remoteURL, registry), nil
		}
		return providers.GitHubGitAuthEnvironment(token, remoteURL, registry), nil
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, err
	}
	workcopies, err := filepath.Abs(layout.WorkcopiesDir())
	if err != nil {
		return nil, err
	}
	gitEnv, err := buildWorktreeGitEnv(cfg, workcopies, project, nil, nil, nil, runner.DefaultRepoCloneURL, registry, stores)
	if err != nil {
		return nil, err
	}
	if gitEnv != nil {
		return gitEnv(ctx, remoteURL)
	}
	return nil, nil
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
