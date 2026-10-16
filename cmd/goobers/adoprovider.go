package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/providerconfig"
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
// scheme the daemon stated beside it (stageADOCredentialKind). The scheme is
// never guessed from the token. A token with no scheme is sent as Basic: that
// is the historical personal-access-token behaviour a standalone invocation
// (GOOBERS_CRED_<cap> set by hand) relies on.
//
// The expiry the daemon delivered beside the token
// (GOOBERS_CREDENTIAL_EXPIRES_<cap>, #5905) lets a 401 say whether the token
// expired or was revoked. A missing or unreadable expiry is ignored: it only
// sharpens that error.
func stageADOCredentialSource(cap capability.Capability, token string) (providers.ADOCredentialSource, error) {
	kind, err := stageADOCredentialKind(cap)
	if err != nil {
		return nil, err
	}
	if refreshing := stageRefreshingToken(cap, token); refreshing != nil {
		// The stage holds a credential-refresh grant (Goobers#6120): the
		// provider's 401 path re-resolves this value once, and it is
		// refreshed ahead of its stated expiry.
		return providers.NewADORefreshingDeliveredCredentialSource(kind, string(cap), refreshing)
	}
	expiresAt, _ := capability.ParseCredentialExpiry(os.Getenv(capability.CredentialExpiryEnvVar(string(cap))))
	return providers.NewADODeliveredCredentialSourceWithExpiry(kind, token, string(cap), expiresAt)
}

// stageADOCredentialKind is the credential kind the token delivered for cap
// is sent as: the scheme the daemon stated for that capability
// (GOOBERS_CREDENTIAL_SCHEME_<cap>, #5957), else the repository's
// (GOOBERS_REPO_AUTH_SCHEME). The per-capability scheme exists because one
// stage's capabilities can come from different sources: a credentials: entry's
// personal access token is Basic on a gaggle whose repository sends a
// Microsoft Entra token as Bearer, and sending it as Bearer earns a 401.
func stageADOCredentialKind(cap capability.Capability) (string, error) {
	name := capability.CredentialSchemeEnvVar(string(cap))
	scheme := strings.TrimSpace(os.Getenv(name))
	if scheme == "" {
		name, scheme = executor.RepoAuthSchemeEnvVar, os.Getenv(executor.RepoAuthSchemeEnvVar)
	}
	kind, err := adoCredentialKindForScheme(scheme)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return kind, nil
}

// adoCredentialKindForScheme maps the authorization scheme the daemon stated
// for an Azure DevOps credential (GOOBERS_CREDENTIAL_SCHEME_<cap> or
// GOOBERS_REPO_AUTH_SCHEME for a stage, the credential plane's per-credential
// scheme or repoAuthScheme for a pod checkout) to the credential kind that
// sends it: "basic", or no stated scheme, is a PAT and "bearer" a Microsoft
// Entra token.
func adoCredentialKindForScheme(scheme string) (string, error) {
	scheme = strings.TrimSpace(scheme)
	switch strings.ToLower(scheme) {
	case "", adoauth.SchemeBasic:
		return providers.ADOCredentialKindPAT, nil
	case adoauth.SchemeBearer:
		return providers.ADOCredentialKindBearer, nil
	default:
		return "", fmt.Errorf("%q is not a supported Azure DevOps authorization scheme (want %q or %q)", scheme, adoauth.SchemeBasic, adoauth.SchemeBearer)
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
// backlog coincide, and whenever no ADO backlog project is declared, the routed
// repo is returned unchanged.
//
// The gaggle's settings come from stageGaggleBacklog: the instance config, or
// in a stage pod the dispatcher's stamp. When they cannot be resolved the
// routed repo is returned with a warning, but that is a defensive fallback
// only: every Azure DevOps stage provider refuses to build in that case
// (requireStageGaggleBacklog), so no work item call goes to the wrong project.
func backlogRepoRefForStage(root string, routed providers.RepositoryRef) providers.RepositoryRef {
	ref, err := stageBacklogRepoRef(root, routed)
	if err != nil {
		warnStageGaggleBacklogUnresolved(err)
	}
	return ref
}

// warnBacklogProjectInvalid reports, once per process, that a gaggle's
// backlog project is malformed and the routed repo is used instead.
func warnBacklogProjectInvalid(gaggle string, err error) {
	if _, warned := stageGaggleConfigWarned.LoadOrStore("backlog project invalid "+gaggle, struct{}{}); warned {
		return
	}
	pf(stageGaggleConfigWarnings, "warning: gaggle %q backlog project is invalid (%v); the routed repository is used\n", gaggle, err)
}

// stageGaggleConfigWarnings is where the stage-side gaggle warnings write; a
// var so tests can capture it.
var stageGaggleConfigWarnings io.Writer = os.Stderr

// stageGaggleConfigWarned remembers the warnings already given, so a stage
// that resolves the same setting many times warns once.
var stageGaggleConfigWarned sync.Map

// warnStageGaggleBacklogUnresolved warns, once per process and reason, that
// a stage's gaggle backlog settings could not be resolved (stageGaggleBacklog)
// on a path that cannot fail the stage itself.
func warnStageGaggleBacklogUnresolved(err error) {
	if _, warned := stageGaggleConfigWarned.LoadOrStore(err.Error(), struct{}{}); warned {
		return
	}
	pf(stageGaggleConfigWarnings, "warning: %v\n", err)
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
	ref, err := providerconfig.ApplyBacklogProject(set, gaggle, routed)
	if err != nil {
		warnBacklogProjectInvalid(gaggle, err)
	}
	return ref
}

// applyGaggleDoneStates sets the ADO provider's predecessor done states from
// the stage's gaggle backlog.doneStates (ADO-N32), resolved by
// stageGaggleBacklog: the instance config, or in a stage pod the dispatcher's
// stamp (#5957). A gaggle that declares none keeps the default: the Resolved,
// Completed and Removed categories. Settings that cannot be resolved are an
// error, never the default: a stricter doneStates silently replaced by the
// default counts a Resolved predecessor as done.
func applyGaggleDoneStates(root string, provider *providers.ADOProvider) error {
	gaggle, set, err := stageGaggleBacklog(root)
	if err != nil || gaggle == "" {
		return err
	}
	if states, ok := providerconfig.GaggleADODoneStates(set, gaggle); ok {
		providers.WithADODoneStates(states)(provider)
	}
	return nil
}
