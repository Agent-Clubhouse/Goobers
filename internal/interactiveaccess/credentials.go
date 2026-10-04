package interactiveaccess

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/githubapp"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

// Credential is callback-scoped secret material, never an HTTP DTO, session
// grant or cached authority. Scheme is bearer for GitHub and basic/bearer for ADO.
type Credential struct {
	Value     string
	Scheme    string
	ExpiresAt time.Time
}

// String prevents accidental credential disclosure through ordinary logging.
func (Credential) String() string { return "[interactive credential redacted]" }

// GoString also redacts diagnostic formatting.
func (Credential) GoString() string { return "[interactive credential redacted]" }

// WithCredential authorizes the exact action and configured target, resolves
// only its explicitly named source, registers the secret before use, and fences
// policy reload through the complete callback. The caller must not retain it.
func (s *Service) WithCredential(ctx context.Context, p httpapi.Principal, gaggle string, action apiv1.InteractiveAction, target Target, use func(context.Context, Credential) error) error {
	if use == nil || !actionTarget(action, target) {
		return ErrDenied
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if err := authorize(p, g, action); err != nil {
		return err
	}
	source, err := selectSource(g, s.sources, target)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resolver, scheme, err := s.resolver(source)
	if err != nil {
		return ErrCredentialUnavailable
	}
	key := "interactive:" + string(action)
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: key, Ref: source.Name}}, s.deps.Registrar)
	if err != nil {
		return err
	}
	set, err := injector.Materialize(ctx, []string{key})
	if err != nil {
		return ErrCredentialUnavailable
	}
	token, err := set.Token(ctx, key)
	if err != nil {
		return ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	expiresAt, _ := set.Expiry(key)
	return use(ctx, Credential{Value: token, Scheme: scheme, ExpiresAt: expiresAt})
}

type resolvedSource struct {
	resolver credentials.Resolver
	scheme   string
}

func (s *Service) resolver(source instance.InteractiveCredential) (credentials.Resolver, string, error) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if cached, ok := s.resolved[source.Name]; ok {
		return cached.resolver, cached.scheme, nil
	}
	resolver, scheme, err := s.buildResolver(source)
	if err == nil {
		s.resolved[source.Name] = resolvedSource{resolver: resolver, scheme: scheme}
	}
	return resolver, scheme, err
}

func (s *Service) buildResolver(source instance.InteractiveCredential) (credentials.Resolver, string, error) {
	repo := source.RepositorySource()
	if source.Token.GitHubCLI != nil && (source.Provider != "github" || source.Token.GitHubCLI.Hostname != "github.com") {
		return nil, "", ErrCredentialUnavailable
	}
	if source.Provider == "ado" {
		provider, err := adoauth.Source(repo, s.deps.Runner, s.deps.Stores)
		if err != nil {
			return nil, "", err
		}
		resolve := func(ctx context.Context) (string, time.Time, error) {
			value, err := provider.Credential(ctx)
			return value.Secret, value.ExpiresAt, err
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, s.deps.Stores, nil, map[string]credentials.ExpiringResolveFunc{source.Name: resolve})
		return resolver, adoauth.AuthScheme(repo), err
	}
	if source.Provider != "github" {
		return nil, "", errors.New("unsupported interactive provider")
	}
	if repo.GitHubAppAuth() {
		app, err := githubapp.Source(repo, s.deps.Registrar, s.deps.Stores)
		if err != nil {
			return nil, "", err
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, s.deps.Stores, nil, map[string]credentials.ExpiringResolveFunc{source.Name: app.DeliverySource(nil)})
		return resolver, "bearer", err
	}
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{source.Token.CredentialTokenRef(source.Name)}, s.deps.Stores)
	return resolver, "bearer", err
}
