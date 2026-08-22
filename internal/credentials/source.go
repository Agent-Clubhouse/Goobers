package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/secfile"
)

// ErrTokenRefNotFound is returned when no TokenRef is registered under the
// requested name.
var ErrTokenRefNotFound = errors.New("credentials: token ref not found")

// ErrTokenRefEmpty is returned when a TokenRef resolves to an empty value —
// treated as misconfiguration, not a valid (empty) secret.
var ErrTokenRefEmpty = errors.New("credentials: token ref resolved to an empty value")

// TokenRef names one secret source: exactly one of Env, File, Keychain, or
// Store must be set. Env/File/Keychain are the tiers 1-2 shape of
// instance.yaml's token refs (docs/ARCHITECTURE.md §6); Store is the tier-3
// counterpart behind the same seam — a "<storeName>/<secretName>" ref into a
// declared external secret store (#683, SEC-010).
type TokenRef struct {
	// Name is the logical name other config (capability grants) refers to
	// this ref by, e.g. "github-issues".
	Name string
	// Env is the environment variable holding the secret value.
	Env string
	// File is the path to a file whose (trimmed) contents are the secret
	// value.
	File string
	// Keychain is the service name of a generic-password item in the macOS
	// login keychain.
	Keychain string
	// Store is a "<storeName>/<secretName>" ref resolved through a
	// StoreResolver. Refs with Store set can only be built into a resolver
	// via NewResolverWithStores — NewResolver fails closed on them.
	Store string
	// GitHubCLI selects a login from the GitHub CLI credential store. The
	// selected token is resolved and identity-checked during resolver creation.
	GitHubCLI *GitHubCLIRef
}

// GitHubCLIRef identifies one authenticated GitHub CLI account.
type GitHubCLIRef struct {
	Hostname string
	User     string
}

func (r TokenRef) validate() error {
	if r.Name == "" {
		return errors.New("credentials: token ref has no name")
	}
	sources := 0
	for _, s := range []string{r.Env, r.File, r.Keychain, r.Store} {
		if s != "" {
			sources++
		}
	}
	if r.GitHubCLI != nil {
		sources++
	}
	if sources != 1 {
		return fmt.Errorf("credentials: token ref %q must set exactly one credential source", r.Name)
	}
	if r.GitHubCLI != nil {
		if strings.TrimSpace(r.GitHubCLI.Hostname) == "" || strings.TrimSpace(r.GitHubCLI.User) == "" {
			return fmt.Errorf("credentials: token ref %q GitHub CLI source requires hostname and user", r.Name)
		}
	}
	return nil
}

// resolve reads the secret value for this ref from the process environment,
// filesystem, macOS Keychain, or the configured secret-store resolver.
func (r TokenRef) resolve(ctx context.Context, stores StoreResolver) (string, error) {
	var raw string
	switch {
	case r.Env != "":
		var ok bool
		raw, ok = os.LookupEnv(r.Env)
		if !ok {
			return "", fmt.Errorf("credentials: token ref %q: env var %q is not set", r.Name, r.Env)
		}
	case r.File != "":
		// Verify the token file is private before reading it, failing closed
		// (secfile rejects on any indeterminate state). Portable across Unix
		// (0600 mode check) and Windows (DACL check) — raw mode bits are
		// meaningless on NTFS. See internal/platform/secfile.
		if err := secfile.VerifyPrivate(r.File); err != nil {
			return "", fmt.Errorf("credentials: token ref %q: %w", r.Name, err)
		}
		b, err := os.ReadFile(r.File)
		if err != nil {
			return "", fmt.Errorf("credentials: token ref %q: read %q: %w", r.Name, r.File, err)
		}
		raw = string(b)
	case r.Keychain != "":
		value, err := resolveKeychain(ctx, r.Keychain)
		if err != nil {
			return "", fmt.Errorf("credentials: token ref %q: %w", r.Name, err)
		}
		raw = value
	case r.Store != "":
		// Construction fails closed on a store ref without a StoreResolver,
		// so stores is never nil here; the guard is belt-and-suspenders.
		if stores == nil {
			return "", fmt.Errorf("credentials: token ref %q: no secret store resolver configured", r.Name)
		}
		value, err := stores.FetchSecret(ctx, r.Store)
		if err != nil {
			return "", fmt.Errorf("credentials: token ref %q: %w", r.Name, err)
		}
		raw = value
	case r.GitHubCLI != nil:
		return resolveGitHubCLI(ctx, *r.GitHubCLI)
	}
	val := strings.TrimSpace(raw)
	if val == "" {
		return "", fmt.Errorf("%w: ref %q", ErrTokenRefEmpty, r.Name)
	}
	return val, nil
}

// Resolver resolves a named secret reference. Implementations must honor
// context cancellation. The local implementation reads env vars, files, and
// macOS Keychain; Azure Key Vault supplies the tier-3 implementation behind
// this same seam (SEC-010), without changes to credential consumers.
type Resolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// StoreResolver resolves a store-backed token ref — the full
// "<storeName>/<secretName>" string from instance.yaml — against the
// instance's declared secret stores (#683). internal/secretstore's Registry
// is the production implementation; the interface keeps this package free of
// any vendor dependency.
type StoreResolver interface {
	FetchSecret(ctx context.Context, ref string) (string, error)
}

// ResolveFunc is a dynamic secret source behind the Resolver seam: a minting
// credential source (e.g. GitHub App installation tokens, #686) that produces
// a value per resolve instead of re-reading an env var or file. Implementations
// own their caching and must honor context cancellation.
type ResolveFunc func(ctx context.Context) (string, error)

// ExpiringResolveFunc is a dynamic source whose values carry a stated expiry
// (GitHub App installation tokens do). The expiry is returned atomically with
// the value it belongs to, which is what lets the credential plane honor DS10
// (distributed-state-and-coordination.md §11): a mint response must carry the
// credential's TTL so a consumer never treats a snapshot as unbounded. A zero
// expiry means the source could not state one for this value.
type ExpiringResolveFunc func(ctx context.Context) (string, time.Time, error)

// DropExpiry adapts f to the plain ResolveFunc seam for consumers that only
// need the value (worktree git auth, providers).
func (f ExpiringResolveFunc) DropExpiry() ResolveFunc {
	return func(ctx context.Context) (string, error) {
		value, _, err := f(ctx)
		return value, err
	}
}

// ExpiringResolver is an optional Resolver refinement: ResolveWithExpiry
// returns the value for name together with its stated expiry when the backing
// source has one (a zero time otherwise). Callers that do not need expiry keep
// using Resolve; the Injector consults this interface structurally so a Set
// can carry per-capability expiry without any wiring changes.
type ExpiringResolver interface {
	Resolver
	ResolveWithExpiry(ctx context.Context, name string) (string, time.Time, error)
}

// tokenRefResolver re-reads env, file, Keychain, and store refs at resolve
// time, while GitHub CLI refs are identity-checked and pinned in memory during
// construction so later ambient account switches cannot change the daemon's
// identity. Dynamic sources are consulted per resolve.
type tokenRefResolver struct {
	refs     map[string]TokenRef
	stores   StoreResolver
	sources  map[string]ResolveFunc
	resolved map[string]string
	// expiring are dynamic sources that state each value's expiry. They share
	// the refs/sources namespace: a name appears in at most one of the three.
	expiring map[string]ExpiringResolveFunc
}

var _ ExpiringResolver = (*tokenRefResolver)(nil)

// NewResolver builds the local env/file/Keychain Resolver from a set of token refs.
// Names must be unique and each ref must be well-formed. A store-backed ref
// fails closed here: local-only construction sites must reject it with a
// diagnostic rather than silently read it as unconfigured — callers that
// support store refs use NewResolverWithStores.
func NewResolver(refs []TokenRef) (Resolver, error) {
	return NewResolverWith(refs, nil, nil)
}

// NewResolverWithStores builds a Resolver whose store-backed refs resolve
// through stores. stores may be nil only when no ref is store-backed;
// otherwise construction fails closed.
func NewResolverWithStores(refs []TokenRef, stores StoreResolver) (Resolver, error) {
	return NewResolverWith(refs, stores, nil)
}

// NewResolverWithSources builds the local Resolver from token refs plus named
// dynamic sources (ResolveFunc). Refs and sources share one namespace — a
// consumer resolves by name without knowing whether the value is read or
// minted — so a name may not appear in both.
func NewResolverWithSources(refs []TokenRef, sources map[string]ResolveFunc) (Resolver, error) {
	return NewResolverWith(refs, nil, sources)
}

// NewResolverWith builds a Resolver from token refs, an optional store resolver
// for store-backed refs (#683), and optional named dynamic sources for minting
// credentials (#686). Refs and dynamic sources share one namespace — a consumer
// resolves by name without knowing whether the value is read or minted — so a
// name may not appear in both. stores may be nil only when no ref is
// store-backed; otherwise construction fails closed.
func NewResolverWith(refs []TokenRef, stores StoreResolver, sources map[string]ResolveFunc) (Resolver, error) {
	return NewResolverWithExpiring(refs, stores, sources, nil)
}

// NewResolverWithExpiring builds a Resolver from token refs, an optional store
// resolver, plain dynamic sources, and expiry-stating dynamic sources
// (ExpiringResolveFunc, e.g. GitHub App installation-token mints). All four
// share one name namespace. The returned Resolver also implements
// ExpiringResolver: ResolveWithExpiry reports the stated expiry for expiring
// sources and a zero time for every other ref kind.
func NewResolverWithExpiring(refs []TokenRef, stores StoreResolver, sources map[string]ResolveFunc, expiring map[string]ExpiringResolveFunc) (Resolver, error) {
	byName := make(map[string]TokenRef, len(refs))
	resolved := make(map[string]string)
	for _, r := range refs {
		if err := r.validate(); err != nil {
			return nil, err
		}
		if r.Store != "" && stores == nil {
			return nil, fmt.Errorf("credentials: token ref %q is store-backed (%q) but no secret store resolver is configured", r.Name, r.Store)
		}
		if _, dup := byName[r.Name]; dup {
			return nil, fmt.Errorf("credentials: duplicate token ref name %q", r.Name)
		}
		if r.GitHubCLI != nil {
			value, err := r.resolve(context.Background(), stores)
			if err != nil {
				return nil, err
			}
			resolved[r.Name] = value
		}
		byName[r.Name] = r
	}
	for name, fn := range sources {
		if name == "" {
			return nil, errors.New("credentials: dynamic source has no name")
		}
		if fn == nil {
			return nil, fmt.Errorf("credentials: dynamic source %q is nil", name)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("credentials: duplicate token ref name %q", name)
		}
	}
	for name, fn := range expiring {
		if name == "" {
			return nil, errors.New("credentials: dynamic source has no name")
		}
		if fn == nil {
			return nil, fmt.Errorf("credentials: dynamic source %q is nil", name)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("credentials: duplicate token ref name %q", name)
		}
		if _, dup := sources[name]; dup {
			return nil, fmt.Errorf("credentials: duplicate token ref name %q", name)
		}
	}
	return &tokenRefResolver{
		refs:     byName,
		stores:   stores,
		sources:  sources,
		resolved: resolved,
		expiring: expiring,
	}, nil
}

// Resolve returns the secret value for the named token ref.
func (r *tokenRefResolver) Resolve(ctx context.Context, name string) (string, error) {
	value, _, err := r.ResolveWithExpiry(ctx, name)
	return value, err
}

// ResolveWithExpiry returns the secret value for the named token ref plus its
// stated expiry when the backing source has one (zero time otherwise).
func (r *tokenRefResolver) ResolveWithExpiry(ctx context.Context, name string) (string, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	if fn, ok := r.expiring[name]; ok {
		value, expiresAt, err := fn(ctx)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("credentials: token ref %q: %w", name, err)
		}
		if strings.TrimSpace(value) == "" {
			return "", time.Time{}, fmt.Errorf("%w: ref %q", ErrTokenRefEmpty, name)
		}
		return value, expiresAt, nil
	}
	if fn, ok := r.sources[name]; ok {
		value, err := fn(ctx)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("credentials: token ref %q: %w", name, err)
		}
		if strings.TrimSpace(value) == "" {
			return "", time.Time{}, fmt.Errorf("%w: ref %q", ErrTokenRefEmpty, name)
		}
		return value, time.Time{}, nil
	}
	ref, ok := r.refs[name]
	if !ok {
		return "", time.Time{}, fmt.Errorf("%w: %q", ErrTokenRefNotFound, name)
	}
	if value, ok := r.resolved[name]; ok {
		return value, time.Time{}, nil
	}
	value, err := ref.resolve(ctx, r.stores)
	return value, time.Time{}, err
}

var githubCLICommand = func(ctx context.Context, hostname, user string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", hostname, "--user", user)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token failed for %s as %s", hostname, user)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return token, nil
}

var githubCLIIdentity = func(ctx context.Context, hostname, token string) (string, error) {
	apiHost := hostname
	path := "/user"
	if hostname == "github.com" {
		apiHost = "api.github.com"
	} else {
		path = "/api/v3/user"
	}
	endpoint := (&url.URL{Scheme: "https", Host: apiHost, Path: path}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build GitHub identity request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("verify GitHub identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verify GitHub identity: status %s", resp.Status)
	}
	var identity struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return "", fmt.Errorf("decode GitHub identity: %w", err)
	}
	if strings.TrimSpace(identity.Login) == "" {
		return "", errors.New("GitHub identity response has no login")
	}
	return identity.Login, nil
}

func resolveGitHubCLI(ctx context.Context, ref GitHubCLIRef) (string, error) {
	hostname := strings.ToLower(strings.TrimSpace(ref.Hostname))
	user := strings.TrimSpace(ref.User)
	token, err := githubCLICommand(ctx, hostname, user)
	if err != nil {
		return "", fmt.Errorf("resolve GitHub CLI credential for %s as %s: %w", hostname, user, err)
	}
	actual, err := githubCLIIdentity(ctx, hostname, token)
	if err != nil {
		return "", fmt.Errorf("verify GitHub CLI credential for %s: %w", user, err)
	}
	if !strings.EqualFold(actual, user) {
		return "", fmt.Errorf("GitHub CLI credential identity mismatch: expected %q, got %q", user, actual)
	}
	return token, nil
}
