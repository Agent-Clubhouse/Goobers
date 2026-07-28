package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	"Detect, stage, and smoke-check a binary target, then hand it to the stable\n" +
	"service supervisor. Policies are manual, on-release (default), and on-main.\n" +
	"Manual requires --target with a release tag; on-main fetches and builds the\n" +
	"configured branch at its resolved commit. This command never changes the\n" +
	"config repository.\n\n" +
	"Flags may also arrive as workflow inputs: policy, owner, repository, branch,\n" +
	"target, healthTicks, healthTimeout, and resultFile.\n"

var prepareSelfUpdate = selfupdate.Prepare

func runSelfUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	policy := fs.String("policy", providerInput("policy", selfupdate.PolicyOnRelease), "update policy: manual, on-release, or on-main")
	owner := fs.String("owner", providerInput("owner", ""), "product repository owner")
	repository := fs.String("repository", providerInput("repository", ""), "product repository name")
	branch := fs.String("branch", providerInput("branch", "main"), "branch tracked by on-main")
	target := fs.String("target", providerInput("target", ""), "manual release tag")
	healthTicks := fs.Int("health-ticks", providerInputInt("healthTicks", selfupdate.DefaultHealthTicks), "required clean heartbeat ticks")
	healthTimeout := fs.Duration("health-timeout", providerInputDuration("healthTimeout", 0), "bounded candidate health window (derived from daemon liveness when omitted)")
	fs.Usage = helpUsage(stderr, "self-update")
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
	if _, err := os.Stat(instance.NewLayout(root).ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", instance.NewLayout(root).ConfigFile())
		return 2
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
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
	if *owner == "" || *repository == "" {
		repo, repoErr := providerRepo(root)
		if repoErr != nil {
			pf(stderr, "error: resolve product repository: %v\n", repoErr)
			return 1
		}
		if *owner == "" {
			*owner = repo.Owner
		}
		if *repository == "" {
			*repository = repo.Name
		}
	}
	workDir, err := os.Getwd()
	if err != nil {
		pf(stderr, "error: resolve working directory: %v\n", err)
		return 1
	}
	token := os.Getenv(executor.CredentialEnvVar(string(capability.ContentsRead)))
	result, err := prepareSelfUpdate(context.Background(), selfupdate.PrepareOptions{
		Root:              root,
		WorkDir:           workDir,
		Policy:            *policy,
		Owner:             *owner,
		Repository:        *repository,
		Branch:            *branch,
		Target:            *target,
		Token:             token,
		RunID:             os.Getenv("GOOBERS_RUN_ID"),
		HealthTicks:       *healthTicks,
		HealthTimeout:     *healthTimeout,
		HeartbeatInterval: heartbeatInterval,
	})
	if err != nil {
		pf(stderr, "error: self-update: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "self-update-result.json")
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: encode self-update result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, append(raw, '\n'), 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	if result.UpdateRequested {
		pf(stdout, "self-update target %s staged; supervisor handoff requested\n", result.Target)
	} else {
		pf(stdout, "self-update target %s is already active\n", result.Target)
	}
	return 0
}

func providerInputInt(key string, fallback int) int {
	value := providerInput(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}

func providerInputDuration(key string, fallback time.Duration) time.Duration {
	value := providerInput(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return -1
	}
	return parsed
}

type selfUpdateEscalator struct {
	root string
}

func (e selfUpdateEscalator) Escalate(ctx context.Context, request selfupdate.Request, reason string) error {
	cfg, err := instance.LoadConfig(instance.NewLayout(e.root).ConfigFile())
	if err != nil {
		return err
	}
	var configured *instance.RepoRef
	for index := range cfg.Repos {
		repo := &cfg.Repos[index]
		if repo.Provider == "github" && repo.Owner == request.Owner && repo.Name == request.Repository {
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
	_, err = provider.CreateWorkItem(ctx, providers.CreateWorkItemRequest{
		Repository: providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    request.Owner,
			Name:     request.Repository,
		},
		Title: "Self-update rolled back: " + request.Target,
		Body: fmt.Sprintf(
			"The supervised Goobers update to `%s` did not complete its clean-health window and was automatically rolled back to the retained previous binary.\n\nReason: %s",
			request.Target,
			reason,
		),
		RunID: runID,
	})
	return err
}

func resolveSelfUpdateEscalationToken(
	ctx context.Context,
	cfg *instance.Config,
	configured instance.RepoRef,
	stores credentials.StoreResolver,
) (string, error) {
	for _, grant := range cfg.Credentials {
		if grant.Capability != string(capability.GitHubIssuesWrite) {
			continue
		}
		name := credentialRefName(grant.Capability)
		resolver, err := credentials.NewResolverWithStores(
			[]credentials.TokenRef{grant.Token.CredentialTokenRef(name)},
			stores,
		)
		if err != nil {
			return "", err
		}
		return resolver.Resolve(ctx, name)
	}
	if configured.GitHubAppAuth() {
		source, err := newGitHubAppTokenSource(configured, nil, stores)
		if err != nil {
			return "", err
		}
		return source(ctx)
	}
	name := configured.Owner + "/" + configured.Name
	resolver, err := credentials.NewResolverWithStores(
		[]credentials.TokenRef{configured.Token.CredentialTokenRef(name)},
		stores,
	)
	if err != nil {
		return "", err
	}
	return resolver.Resolve(ctx, name)
}

var _ selfupdate.Escalator = selfUpdateEscalator{}
