package terminalwriter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// AppliedConfig returns the control role's applied instance configuration and
// the digest of the generation it came from; ok is false before any applied.
type AppliedConfig func() (cfg *instance.Config, digest string, ok bool)

// ErrNoAppliedConfig is returned until the control role applies a
// configuration. It is transient.
var ErrNoAppliedConfig = errors.New("no instance configuration is applied")

// Registrar is where every credential form a request sends is registered, and
// the scrubber that redacts them; journal.RegistryScrubber satisfies it.
type Registrar interface {
	credentials.SecretRegistrar
	providers.SecretRegistrar
}

// ConfiguredBackends builds Backends from the applied instance configuration.
// Credentials are scoped to the bound gaggle's configured repository and
// materialized per capability; they are rebuilt when the applied digest moves.
type ConfiguredBackends struct {
	applied   AppliedConfig
	registrar Registrar
	stores    func(*instance.Config) (credentials.StoreResolver, error)

	mu        sync.Mutex
	digest    string
	injectors map[string]scopedCredentials
}

type scopedCredentials struct {
	injector *credentials.Injector
	grants   []credentials.Grant
}

var _ Backends = (*ConfiguredBackends)(nil)

// NewConfiguredBackends returns Backends over applied. stores builds the
// secret-store resolver for a configuration (secretstore.NewRegistry).
func NewConfiguredBackends(applied AppliedConfig, registrar Registrar, stores func(*instance.Config) (credentials.StoreResolver, error)) (*ConfiguredBackends, error) {
	if applied == nil || registrar == nil || stores == nil {
		return nil, errors.New("configured backends require an applied configuration, a registrar and a store resolver")
	}
	return &ConfiguredBackends{applied: applied, registrar: registrar, stores: stores}, nil
}

// For returns repo's Backend authenticated for capability.
func (b *ConfiguredBackends) For(ctx context.Context, bound Binding, repo providers.RepositoryRef, capability string) (Backend, error) {
	cfg, digest, ok := b.applied()
	if !ok || cfg == nil {
		return nil, ErrNoAppliedConfig
	}
	configured, err := configuredRepo(cfg, bound.Project)
	if err != nil {
		return nil, fmt.Errorf("gaggle %q: %w", bound.Gaggle, err)
	}
	if repo.Provider == providers.ProviderADO && !sameRepository(repo, bound.Workspace) {
		// Azure Boards: the configured repository's organization-scoped auth
		// addresses the backlog project (adoauth), as the daemon's release does.
		stores, err := b.stores(cfg)
		if err != nil {
			return nil, err
		}
		provider, err := adoauth.Provider(configured, nil, b.registrar, nil, nil, stores)
		if err != nil {
			return nil, err
		}
		return adoBackend{provider}, nil
	}
	scoped, err := b.credentials(cfg, digest, bound, repo, configured)
	if err != nil {
		return nil, err
	}
	set, err := scoped.injector.Materialize(ctx, []string{capability})
	if err != nil {
		return nil, err
	}
	source := set.For(capability)
	switch repo.Provider {
	case providers.ProviderGitHub:
		var opts []func(*providers.GitHubProvider)
		opts = append(opts, providers.WithTokenSource(source))
		if repo.URL != "" {
			opts = append(opts, func(p *providers.GitHubProvider) { p.BaseURL = repo.URL })
		}
		if login := cfg.GitHubBotLogin(repo.Owner, repo.Name); login != "" {
			opts = append(opts, providers.WithConfiguredLogin(login))
		}
		return githubBackend(providers.NewGitHubProvider("", opts...)), nil
	case providers.ProviderGitea:
		if configured.BaseURL == "" {
			return nil, fmt.Errorf("gitea repo %s/%s has no baseUrl configured", configured.Owner, configured.Name)
		}
		return giteaBackend(providers.NewGiteaProvider(configured.BaseURL, "", providers.WithGiteaTokenSource(source))), nil
	case providers.ProviderADO:
		scheme := runtimewiring.AdoCredentialSchemes(cfg, bound.Project, scoped.grants)[capability]
		if scheme == "" {
			scheme = adoauth.AuthScheme(configured)
		}
		kind, err := adoCredentialKind(scheme)
		if err != nil {
			return nil, err
		}
		credential, err := providers.NewADOTokenSourceCredentialSource(kind, source)
		if err != nil {
			return nil, err
		}
		return adoBackend{providers.NewADOProvider(repo.Owner, repo.Project, "",
			providers.WithADOCredentialSource(credential), providers.WithADOSecretRegistrar(b.registrar))}, nil
	}
	return nil, fmt.Errorf("repository provider %q: %w", repo.Provider, ErrUnsupported)
}

// configuredRepo is the applied config's entry for project, matched on its
// whole identity, host included (runtimewiring.ConfiguredRepoForProject
// matches owner/name only): an identically named repository on another host
// must never supply its credentials. Exactly one entry must match. An ADO
// project with no exact entry uses the gaggle's organization mapping.
func configuredRepo(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, error) {
	want := repositoryRef(project)
	var matched []instance.RepoRef
	for _, configured := range cfg.Repos {
		candidate := providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), URL: configured.BaseURL, Owner: configured.Owner, Project: configured.Project, Name: configured.Name}
		if candidate.CanonicalKey() == want.CanonicalKey() {
			matched = append(matched, configured)
		}
	}
	switch {
	case len(matched) == 1:
		return matched[0], nil
	case len(matched) == 0 && project.Provider == apiv1.ProviderADO:
		if repo, ok := runtimewiring.AdoRepoForGaggle(cfg, project); ok {
			return repo, nil
		}
	}
	return instance.RepoRef{}, fmt.Errorf("project %s matches %d configured repositories, want exactly one", want.CanonicalKey(), len(matched))
}

// credentials returns the injector scoped to the bound gaggle's configured
// repository, reusing it while the applied digest is unchanged.
func (b *ConfiguredBackends) credentials(cfg *instance.Config, digest string, bound Binding, repo providers.RepositoryRef, configured instance.RepoRef) (scopedCredentials, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if digest != b.digest {
		b.digest, b.injectors = digest, map[string]scopedCredentials{}
	}
	key := repositoryRef(bound.Project).CanonicalKey() + "\x00" + repo.CanonicalKey()
	if scoped, ok := b.injectors[key]; ok {
		return scoped, nil
	}
	// Only the pinned repository's own entry may supply credentials, so an
	// identically named repository on another host cannot.
	narrowed := *cfg
	narrowed.Repos = []instance.RepoRef{configured}
	stores, err := b.stores(cfg)
	if err != nil {
		return scopedCredentials{}, err
	}
	var resolver credentials.Resolver
	var grants []credentials.Grant
	if repo.Provider != providers.ProviderKind(bound.Project.Provider) {
		// A backlog on another provider than the code (topology (b)) uses the
		// gaggle's backlog role credential.
		resolver, grants, err = runtimewiring.BuildRoleCredentials(cfg, stores, runtimewiring.CredentialBindingOwner(bound.Project), bound.Project.Name, runtimewiring.BacklogRoleFor(repo.Owner, repo.Name), nil, b.registrar)
	} else {
		resolver, grants, err = runtimewiring.BuildCredentials(&narrowed, stores, runtimewiring.CredentialBindingOwner(bound.Project), bound.Project.Name, nil, b.registrar)
	}
	if err != nil {
		return scopedCredentials{}, err
	}
	injector, err := credentials.NewInjector(resolver, grants, b.registrar)
	if err != nil {
		return scopedCredentials{}, err
	}
	scoped := scopedCredentials{injector: injector, grants: grants}
	b.injectors[key] = scoped
	return scoped, nil
}

func adoCredentialKind(scheme string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "", adoauth.SchemeBasic:
		return providers.ADOCredentialKindPAT, nil
	case adoauth.SchemeBearer:
		return providers.ADOCredentialKindBearer, nil
	}
	return "", fmt.Errorf("%q is not a supported Azure DevOps authorization scheme", scheme)
}
