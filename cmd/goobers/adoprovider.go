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
	"github.com/goobers/goobers/internal/telemetry"
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
	telemetryOpt := providers.WithADORateLimitObserver(
		telemetry.NewStageRateLimitObserver(os.Getenv(telemetry.StageTelemetryEnv)),
	)
	return providers.NewADOProvider(routed.Owner, routed.Project, "", providers.WithADOCredentialSource(credential), telemetryOpt), nil
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
	return adoCredentialKindForScheme(os.Getenv(executor.RepoAuthSchemeEnvVar))
}

// adoCredentialKindForScheme maps the authorization scheme the daemon stated
// for an Azure DevOps repository credential (GOOBERS_REPO_AUTH_SCHEME for a
// stage, the credential plane's repoAuthScheme for a pod checkout) to the
// credential kind that sends it: "basic", or no stated scheme, is a PAT and
// "bearer" a Microsoft Entra token.
func adoCredentialKindForScheme(scheme string) (string, error) {
	scheme = strings.TrimSpace(scheme)
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

// applyBacklogProject resolves the ref the named gaggle's backlog role
// addresses, given the routed code repository.
//
// For an ADO backlog on an ADO project it overrides only the project tier of
// routed with the backlog project. Organization, name, and credentials stay
// the routed code repo's (the ADO provider is org-scoped and the backlog lives
// under the same organization and auth).
//
// For a GitHub or Gitea backlog on an ADO project (topology (b),
// docs/design/ado-parity-dsl-2-0.md §7.2) it returns the backlog provider's
// own ref (backlogProviderRef), so backlog work opens that provider.
//
// It returns routed unchanged when the gaggle is absent, the backlog project
// is undeclared, or the topology is not one this release routes by role.
func applyBacklogProject(set *instance.ConfigSet, gaggle string, routed providers.RepositoryRef) providers.RepositoryRef {
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		backlog := g.Spec.Backlog
		if backlog.Project == "" {
			return routed
		}
		if crossProviderBacklog(apiv1.Provider(routed.Provider), backlog.Provider) {
			ref, err := backlogProviderRef(g.Name, g.Spec.Project, backlog)
			if err != nil {
				return routed
			}
			return ref
		}
		if backlog.Provider != apiv1.ProviderADO {
			return routed
		}
		ref := routed
		ref.Project = backlog.Project
		return ref
	}
	return routed
}

// crossProviderBacklog reports whether a gaggle whose code lives on project
// keeps its backlog on backlog, a different provider, in a topology that is
// routed by role: a GitHub or Gitea backlog for Azure DevOps code (topology
// (b), docs/design/ado-parity-dsl-2-0.md §7.2). Issue work then opens the
// backlog provider and pull-request work the project provider. Every other
// combination keeps the single routed provider it always used.
func crossProviderBacklog(project, backlog apiv1.Provider) bool {
	if project != apiv1.ProviderADO {
		return false
	}
	return backlog == apiv1.ProviderGitHub || backlog == apiv1.ProviderGitea
}

// backlogProviderRef builds the RepositoryRef of a gaggle's backlog provider
// from its spec: owner/name from backlog.project on GitHub and Gitea, the
// backlog project on Azure DevOps, and backlog.baseUrl as the service URL.
func backlogProviderRef(gaggle string, project apiv1.RepoRef, backlog apiv1.BacklogRef) (providers.RepositoryRef, error) {
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(backlog.Provider),
		Owner:    project.Owner,
		Project:  project.Project,
		Name:     project.Name,
		URL:      backlog.BaseURL,
	}
	switch repo.Provider {
	case providers.ProviderGitHub, providers.ProviderGitea:
		owner, name, ok := strings.Cut(backlog.Project, "/")
		if !ok || owner == "" || name == "" {
			return providers.RepositoryRef{}, fmt.Errorf(
				"gaggle %q backlog project %q must be owner/name",
				gaggle,
				backlog.Project,
			)
		}
		// The project tier is Azure DevOps addressing; a GitHub or Gitea
		// repository has none.
		repo.Owner, repo.Project, repo.Name = owner, "", name
	case providers.ProviderADO:
		repo.Project = backlog.Project
	}
	return repo, nil
}

// backlogProviderRepo is the repository a stage opens its backlog provider
// for. A backlog on the routed provider (GitHub, or the ADO project split)
// keeps opening routed exactly as before and only addresses backlog in its
// work-item calls; a backlog on another provider (topology (b)) opens that
// provider.
func backlogProviderRepo(routed, backlog providers.RepositoryRef) providers.RepositoryRef {
	if backlogOnOtherProvider(routed, backlog) {
		return backlog
	}
	return routed
}

// backlogOnOtherProvider reports whether backlog work leaves the routed
// provider: topology (b), a GitHub or Gitea backlog for Azure DevOps code
// (crossProviderBacklog).
func backlogOnOtherProvider(routed, backlog providers.RepositoryRef) bool {
	return crossProviderBacklog(apiv1.Provider(routed.Provider), apiv1.Provider(backlog.Provider))
}

// applyGaggleDoneStates sets the ADO provider's predecessor done states from
// the stage's gaggle backlog.doneStates (ADO-N32). It resolves the gaggle the
// same way backlogRepoRefForStage does. When the gaggle, its config or the
// setting cannot be resolved (for example in a stage pod, which has no
// instance config), the provider keeps its default: the Resolved, Completed
// and Removed categories.
func applyGaggleDoneStates(root string, provider *providers.ADOProvider) {
	gaggle := os.Getenv(executor.GaggleEnvVar)
	if gaggle == "" {
		return
	}
	set, report, err := instance.LoadConfigDir(layoutFor(root).ConfigDir())
	if err != nil || report == nil || set == nil {
		return
	}
	if states, ok := gaggleADODoneStates(set, gaggle); ok {
		providers.WithADODoneStates(states)(provider)
	}
}

// gaggleADODoneStates converts the named gaggle's backlog.doneStates into the
// provider form. It reports false when the gaggle is absent, its backlog is
// not ADO, or it declares no doneStates.
func gaggleADODoneStates(set *instance.ConfigSet, gaggle string) (providers.ADODoneStates, bool) {
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		if g.Name != gaggle {
			continue
		}
		backlog := g.Spec.Backlog
		if backlog.Provider != apiv1.ProviderADO || backlog.DoneStates == nil {
			return providers.ADODoneStates{}, false
		}
		states := providers.ADODoneStates{ByType: backlog.DoneStates.ByType}
		for _, category := range backlog.DoneStates.Categories {
			states.Categories = append(states.Categories, string(category))
		}
		return states, true
	}
	return providers.ADODoneStates{}, false
}
