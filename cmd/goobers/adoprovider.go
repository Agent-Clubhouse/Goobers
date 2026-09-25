package main

import (
	"fmt"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// adoRepoRefForConfig resolves the instance ADO RepoRef matching the routed
// repository (owner/project/name) in the instance config. A single-ADO-repo
// instance falls back to its only repo. The returned RepoRef carries the auth
// block (azure-cli/PAT/workload/managed identity) a configured credential
// source needs. Only processes that are not stages read it (see
// newConfiguredADOProvider); a stage authenticates with the credential the
// daemon delivered for its declared capability instead.
func adoRepoRefForConfig(root string, routed providers.RepositoryRef) (instance.RepoRef, error) {
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return instance.RepoRef{}, err
	}
	for _, repo := range cfg.Repos {
		if repo.Provider != string(providers.ProviderADO) {
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
		if repo.Provider == string(providers.ProviderADO) && repo.Owner == routed.Owner && repo.Name == routed.Name {
			return repo, nil
		}
	}
	if len(cfg.Repos) == 1 && cfg.Repos[0].Provider == string(providers.ProviderADO) {
		return cfg.Repos[0], nil
	}
	return instance.RepoRef{}, fmt.Errorf("no ADO repo %s/%s/%s configured in %s", routed.Owner, routed.Project, routed.Name, l.ConfigFile())
}

// newConfiguredADOProvider builds an ADO provider from the repository's
// configured authentication in instance.yaml. It serves the processes that
// are not stages and hold the instance config: the daemon (the runner's
// escalation comments) and operator commands that opt in with
// withStageProviderConfiguredADOAuth (goobers run, goobers status). A stage
// never uses it, so a stage on Azure DevOps authenticates only with what its
// declared capabilities delivered (docs/design/ado-parity-dsl-2-0.md §3.1).
var newConfiguredADOProvider = buildConfiguredADOProvider

func buildConfiguredADOProvider(root string, routed providers.RepositoryRef) (*providers.ADOProvider, error) {
	repo, err := adoRepoRefForConfig(root, routed)
	if err != nil {
		return nil, err
	}
	return adoauth.Provider(repo, nil, nil, nil, nil, nil)
}

// newADOProviderForStage builds the ADO provider a stage talks to from
// credential, the value the daemon delivered for the capability the stage
// declared (stageADOCredentialSource). It reads no instance config, so it
// works the same in a stage pod, which has none. A package var so tests point
// the provider at a fake server and observe which credential it was given.
var newADOProviderForStage = buildADOProviderForStage

func buildADOProviderForStage(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
	if credential == nil {
		return nil, fmt.Errorf("ADO stage provider for %s/%s/%s has no credential", routed.Owner, routed.Project, routed.Name)
	}
	return providers.NewADOProvider(routed.Owner, routed.Project, "", providers.WithADOCredentialSource(credential)), nil
}

// stageADOCredentialSource turns the token delivered for cap
// (GOOBERS_CRED_<cap>) into an Azure DevOps credential, in the authorization
// scheme the daemon stated beside it (executor.RepoAuthSchemeEnvVar). The
// scheme is never guessed from the token. A token with no scheme is sent as
// Basic: that is the historical personal-access-token behaviour a standalone
// invocation (GOOBERS_CRED_<cap> set by hand) relies on.
func stageADOCredentialSource(cap capability.Capability, token string) (providers.ADOCredentialSource, error) {
	kind, err := stageADOCredentialKind()
	if err != nil {
		return nil, err
	}
	return providers.NewADODeliveredCredentialSource(kind, token, string(cap))
}

func stageADOCredentialKind() (string, error) {
	scheme := strings.TrimSpace(os.Getenv(executor.RepoAuthSchemeEnvVar))
	switch strings.ToLower(scheme) {
	case "", adoauth.SchemeBasic:
		return providers.ADOCredentialKindPAT, nil
	case adoauth.SchemeBearer:
		return providers.ADOCredentialKindBearer, nil
	default:
		return "", fmt.Errorf("%s=%q is not a supported Azure DevOps authorization scheme (want %q or %q)", executor.RepoAuthSchemeEnvVar, scheme, adoauth.SchemeBasic, adoauth.SchemeBearer)
	}
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
	gaggle := os.Getenv(executor.GaggleEnvVar)
	if gaggle == "" {
		return routed
	}
	set, report, err := instance.LoadConfigDir(layoutFor(root).ConfigDir())
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
