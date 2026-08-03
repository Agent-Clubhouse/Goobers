package main

import (
	"fmt"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
// operations of a provider-chain stage must address. On Azure DevOps the code
// repository a run targets (gaggle.spec.project — where branches and PRs land,
// e.g. "Example Service") is a *different ADO project* from the backlog the
// gaggle draws PBIs from (gaggle.spec.backlog — e.g. "Example Backlog"). Work-item WIQL
// and REST are project-scoped ([System.TeamProject] = @project), so a backlog
// list/claim/close must target the backlog project, not the routed code-repo
// project — otherwise the query runs against the wrong (usually far larger)
// project and returns no matching PBIs. Only the project tier differs: the ADO
// provider is organization-scoped and the backlog project lives under the same
// organization (and the same azure-cli/PAT auth), so organization, name, and
// credentials stay the routed code repo's. On GitHub, where the code repo and
// backlog coincide, and whenever no ADO backlog project is declared or the
// gaggle can't be resolved, the routed repo is returned unchanged.
func backlogRepoRefForStage(root string, routed providers.RepositoryRef) providers.RepositoryRef {
	if routed.Provider != providers.ProviderADO {
		return routed
	}
	gaggle := os.Getenv("GOOBERS_GAGGLE")
	if gaggle == "" {
		return routed
	}
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	if err != nil || report == nil || set == nil {
		return routed
	}
	return applyBacklogProject(set, gaggle, routed)
}

// backlogRepoRefForGaggle is the daemon-side counterpart of
// backlogRepoRefForStage: the run's failure/park/escalation handlers execute in
// the daemon process (not a routed stage subprocess), so the gaggle name comes
// from the gaggle-scoped layout (instance.Layout.ForGaggle) rather than the
// GOOBERS_GAGGLE stage-env var. It applies the same code-repo→backlog-project
// override so an ADO work-item mutation (park needs-human, release claim, leave
// a failure comment) targets the backlog project (e.g. "Example Backlog") the PBI actually
// lives in, not the code-repo project the branch/PR landed in.
func backlogRepoRefForGaggle(l instance.Layout, routed providers.RepositoryRef) providers.RepositoryRef {
	if routed.Provider != providers.ProviderADO {
		return routed
	}
	gaggle := l.Gaggle()
	if gaggle == "" {
		return routed
	}
	set, report, err := instance.LoadConfigDir(l.ConfigDir())
	if err != nil || report == nil || set == nil {
		return routed
	}
	return applyBacklogProject(set, gaggle, routed)
}

// applyBacklogProject overrides only the project tier of routed with the named
// gaggle's ADO backlog project. Organization, name, and credentials stay the
// routed code repo's (the ADO provider is org-scoped and the backlog lives
// under the same organization and auth). Returns routed unchanged when the
// gaggle is absent, its backlog is not ADO, or no backlog project is declared.
func applyBacklogProject(set *instance.ConfigSet, gaggle string, routed providers.RepositoryRef) providers.RepositoryRef {
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		backlog := g.Spec.Backlog
		if backlog.Provider != apiv1.ProviderADO || backlog.Project == "" {
			return routed
		}
		ref := routed
		ref.Project = backlog.Project
		return ref
	}
	return routed
}
