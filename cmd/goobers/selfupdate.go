package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/providers"
)

const selfUpdateHelp = "Usage: goobers self-update [flags] [path]\n\n" +
	"Stage and smoke-check a binary, then request supervised activation. Policies\n" +
	"are manual, on-release (default), and on-main. Manual requires a release tag;\n" +
	"on-main builds the configured branch. on-release resolves the newest stable\n" +
	"release unless --include-prerelease is set, which considers all GitHub\n" +
	"releases and only stages a target strictly newer than the running build.\n"

func runSelfUpdate(args []string, stdout, stderr io.Writer) int {
	return runSelfUpdateWith(args, stdout, stderr, "self-update", selfupdate.Prepare)
}

func runSelfUpdateWith(
	args []string,
	stdout, stderr io.Writer,
	command string,
	prepare func(context.Context, selfupdate.PrepareOptions) (selfupdate.PrepareResult, error),
) int {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	policy := fs.String("policy", providerInput("policy", selfupdate.PolicyOnRelease), "update policy: manual, on-release, or on-main")
	includePrerelease := fs.Bool("include-prerelease", false, "for on-release, include GitHub pre-releases when selecting the newest target")
	branch := fs.String("branch", providerInput("branch", "main"), "branch tracked by on-main")
	target := fs.String("target", providerInput("target", ""), "manual release tag")
	healthTicks := fs.Int("health-ticks", selfupdate.DefaultHealthTicks, "required clean heartbeat ticks")
	healthTimeout := fs.Duration("health-timeout", 0, "bounded candidate health window (derived from daemon liveness when omitted)")
	fs.Usage = helpUsage(stderr, command)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := providerStageRoot("")
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		pf(stderr, "error: resolve instance root: %v\n", err)
		return 1
	}
	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", layout.ConfigFile())
		return 2
	}
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 1
	}
	livenessTimeout, err := cfg.Runner.LivenessTimeoutDuration()
	if err != nil {
		pf(stderr, "error: resolve daemon heartbeat interval: %v\n", err)
		return 1
	}
	heartbeatInterval := livenessTimeout / 2
	if *healthTimeout == 0 {
		*healthTimeout = selfupdate.DefaultHealthTimeout
		minimumWindow := time.Duration(*healthTicks+1) * heartbeatInterval
		if *healthTimeout < minimumWindow {
			*healthTimeout = minimumWindow
		}
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: resolve product repository: %v\n", err)
		return 1
	}
	owner, repository := providerInput("owner", repo.Owner), providerInput("repository", repo.Name)
	workDir, err := os.Getwd()
	if err != nil {
		pf(stderr, "error: resolve working directory: %v\n", err)
		return 1
	}
	result, err := prepare(context.Background(), selfupdate.PrepareOptions{
		Root:              root,
		WorkDir:           workDir,
		Policy:            *policy,
		IncludePrerelease: *includePrerelease,
		Owner:             owner,
		Repository:        repository,
		Branch:            *branch,
		Target:            *target,
		Token:             os.Getenv(executor.CredentialEnvVar(string(capability.ContentsRead))),
		RunID:             os.Getenv(executor.RunIDEnvVar),
		HealthTicks:       *healthTicks,
		HealthTimeout:     *healthTimeout,
		HeartbeatInterval: heartbeatInterval,
	})
	if err != nil {
		pf(stderr, "error: self-update: %v\n", err)
		return 1
	}
	if err := writeProviderStageResult(providerInput("resultFile", "self-update-result.json"), map[string]interface{}{
		"updateRequested": result.UpdateRequested,
		"policy":          result.Policy,
		"target":          result.Target,
	}); err != nil {
		pf(stderr, "error: write self-update result: %v\n", err)
		return 1
	}
	if result.UpdateRequested {
		pf(stdout, "self-update target %s staged; supervisor handoff requested\n", result.Target)
	} else {
		pf(stdout, "self-update target %s is already active\n", result.Target)
	}
	return 0
}

type selfUpdateEscalator struct{ root string }

func (e selfUpdateEscalator) Escalate(ctx context.Context, request selfupdate.Request, reason string) error {
	cfg, err := instance.LoadConfig(instance.NewLayout(e.root).ConfigFile())
	if err != nil {
		return err
	}
	var configured *instance.RepoRef
	for index := range cfg.Repos {
		repo := &cfg.Repos[index]
		if repo.Provider == string(providers.ProviderGitHub) && repo.Owner == request.Owner && repo.Name == request.Repository {
			configured = repo
			break
		}
	}
	if configured == nil {
		return fmt.Errorf("rollback repository %s/%s is not configured in instance.yaml", request.Owner, request.Repository)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return err
	}
	token, err := resolveSelfUpdateEscalationToken(ctx, cfg, *configured, stores)
	if err != nil {
		return err
	}
	provider := newGitHubProvider(token)
	runID := request.RunID
	if runID == "" {
		runID = "self-update-" + request.Target
	}
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: request.Owner, Name: request.Repository}
	_, err = provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
		Repository: repository,
		Title:      "Self-update rolled back: " + request.Target,
		Body:       fmt.Sprintf("The supervised update to `%s` was rolled back.\n\nReason: %s", request.Target, reason),
		RunID:      runID,
	})
	return err
}

func resolveSelfUpdateEscalationToken(ctx context.Context, cfg *instance.Config, configured instance.RepoRef, stores credentials.StoreResolver) (string, error) {
	for _, grant := range cfg.Credentials {
		if grant.Capability != string(capability.GitHubIssuesWrite) {
			continue
		}
		name := credentialRefName(grant.Capability)
		resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{grant.Token.CredentialTokenRef(name)}, stores)
		if err != nil {
			return "", err
		}
		return resolver.Resolve(ctx, name)
	}
	return resolveRepoToken(configured, configured.Owner+"/"+configured.Name, stores)
}
