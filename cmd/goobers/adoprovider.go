package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// adoRepoRefForStage resolves the instance ADO RepoRef a provider-chain stage
// operates against, matching the scheduler-routed repository (owner/project/
// name) against the instance config. A single-ADO-repo instance falls back to
// its only repo. The returned RepoRef carries the auth block (azure-cli/PAT/
// workload/managed identity) the credential source needs — the routed env only
// carries the addressing tuple, not the auth configuration.
func adoRepoRefForStage(root string, routed providers.RepositoryRef) (instance.RepoRef, error) {
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return instance.RepoRef{}, err
	}
	for _, repo := range cfg.Repos {
		if repo.Provider != "ado" {
			continue
		}
		if repo.Owner == routed.Owner && repo.Project == routed.Project && repo.Name == routed.Name {
			return repo, nil
		}
	}
	// Fall back to an owner+name match ignoring project: a work-item call routed
	// to the gaggle's backlog project (e.g. "Example Backlog") carries a different project
	// than the code-repo config entry (e.g. "Example Service"), yet the same
	// config entry's org-scoped auth serves it — the ADO provider is
	// organization-scoped and the project is only per-call addressing.
	for _, repo := range cfg.Repos {
		if repo.Provider == "ado" && repo.Owner == routed.Owner && repo.Name == routed.Name {
			return repo, nil
		}
	}
	if len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "ado" {
		return cfg.Repos[0], nil
	}
	return instance.RepoRef{}, fmt.Errorf("no ADO repo %s/%s/%s configured in %s", routed.Owner, routed.Project, routed.Name, l.ConfigFile())
}

// newADOProviderForStage builds the ADO provider a provider-chain stage talks
// to using its configured authentication source.
var newADOProviderForStage = buildADOProviderForStage

func buildADOProviderForStage(root string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
	repo, err := adoRepoRefForStage(root, routed)
	if err != nil {
		return nil, err
	}
	return adoauth.Provider(repo, nil, nil, nil, nil, nil)
}

// open-pr receives PAT credentials through its provider:pr:write capability;
// the configured PAT environment variable is intentionally absent from the
// stage's default-deny environment.
var newADOProviderForOpenPR = buildADOProviderForOpenPR

func buildADOProviderForOpenPR(root string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
	repo, err := adoRepoRefForStage(root, routed)
	if err != nil {
		return nil, err
	}
	kind := instance.ADOAuthPAT
	if repo.Auth != nil {
		kind = repo.Auth.Kind
	}
	if kind == instance.ADOAuthPAT {
		repo.Token = instance.TokenRef{Env: executor.CredentialEnvVar(string(capability.ProviderPRWrite))}
	}
	return adoauth.Provider(repo, nil, nil, nil, nil, nil)
}

// backlogRepoRefForStage resolves the RepositoryRef the work-item (backlog)
// operations of a provider-chain stage must address. The backlog a gaggle draws
// from (gaggle.spec.backlog) is modeled independently of the code repository a
// run targets (gaggle.spec.project): on Azure DevOps they are different
// projects within one organization ("Example Backlog" vs "Example Service"),
// and since personal-gaggle-routing they may be an entirely different
// repository or even a different provider — a gaggle targeting repo A can
// consume a private backlog in repo B.
//
// Resolution is delegated to stageBacklogRef (backlogidentity.go) so the
// addressing ref, the authoritative claim scope, and the credential connection
// are all derived from the same resolved spec.backlog. A resolution failure
// returns the routed repo unchanged, preserving the historical
// same-project/same-backlog behavior for every gaggle that declares no distinct
// backlog; callers that must not silently address the wrong container use
// backlogIdentityForStage, which fails loudly instead.
func backlogRepoRefForStage(root string, routed providers.RepositoryRef) providers.RepositoryRef {
	ref, _, err := stageBacklogRef(root, routed)
	if err != nil {
		return routed
	}
	backlogRepo, err := backlogRepositoryRef(ref, routed)
	if err != nil {
		return routed
	}
	return backlogRepo
}

// backlogRepoRefForGaggle is the daemon-side counterpart of
// backlogRepoRefForStage: the run's failure/park/escalation handlers execute in
// the daemon process (not a routed stage subprocess), so the gaggle name comes
// from the gaggle-scoped layout (instance.Layout.ForGaggle) rather than the
// GOOBERS_GAGGLE stage-env var.
func backlogRepoRefForGaggle(l instance.Layout, routed providers.RepositoryRef) providers.RepositoryRef {
	ref, _ := gaggleBacklogRef(l, routed)
	backlogRepo, err := backlogRepositoryRef(ref, routed)
	if err != nil {
		return routed
	}
	return backlogRepo
}
