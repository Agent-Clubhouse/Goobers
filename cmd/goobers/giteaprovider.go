package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// giteaRepoRefForStage resolves the instance Gitea RepoRef a provider-chain
// stage operates against, matching the scheduler-routed repository (owner/name)
// against the instance config. A single-Gitea-repo instance falls back to its
// only repo. The returned RepoRef carries BaseURL — the self-hosted forge root
// the routed env does not carry (the routed env only carries the addressing
// tuple) — which newGiteaProviderForStage needs to reach the right host. Mirrors
// adoRepoRefForStage, but Gitea's code repo and backlog coincide (like GitHub),
// so there is no project tier to reconcile.
func giteaRepoRefForStage(root string, routed providers.RepositoryRef) (instance.RepoRef, error) {
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return instance.RepoRef{}, err
	}
	for _, repo := range cfg.Repos {
		if repo.Provider != string(providers.ProviderGitea) {
			continue
		}
		if repo.Owner == routed.Owner && repo.Name == routed.Name {
			return repo, nil
		}
	}
	if len(cfg.Repos) == 1 && cfg.Repos[0].Provider == string(providers.ProviderGitea) {
		return cfg.Repos[0], nil
	}
	return instance.RepoRef{}, fmt.Errorf("no gitea repo %s/%s configured in %s", routed.Owner, routed.Name, l.ConfigFile())
}

// newGiteaProviderForStage builds the Gitea provider a provider-chain stage
// talks to. It mirrors newADOProviderForStage in resolving the forge BaseURL
// from instance config, but — like GitHub — Gitea authenticates with a static
// PAT-like token the runner injects for the stage's declared capability
// (providerToken). The token is resolved by the caller (the stage's own
// capability, or the daemon's credential resolver) and passed in, so this stays
// covered by the run's registrar-based secret scrubbing rather than becoming a
// second unregistered copy of the secret. The stage rate-limit telemetry
// observer newTelemetryGitHubProvider wires is wired here too; extra opts let a
// call site add a mutation recorder for the journal.
func newGiteaProviderForStage(root string, routed providers.RepositoryRef, token string, opts ...func(*providers.GiteaProvider)) (*providers.GiteaProvider, error) {
	repo, err := giteaRepoRefForStage(root, routed)
	if err != nil {
		return nil, err
	}
	if repo.BaseURL == "" {
		return nil, fmt.Errorf("gitea repo %s/%s has no baseUrl configured", routed.Owner, routed.Name)
	}
	telemetryOpt := providers.WithGiteaRateLimitObserver(
		telemetry.NewStageRateLimitObserver(os.Getenv(telemetry.StageTelemetryEnv)),
	)
	return providers.NewGiteaProvider(repo.BaseURL, token, append([]func(*providers.GiteaProvider){telemetryOpt}, opts...)...), nil
}
