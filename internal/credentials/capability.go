package credentials

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrUndeclaredCapability is returned when a caller asks for a credential
// for a capability that was not in the declared set a Set was materialized
// for. It is the runtime backstop behind capability admission (SEC-042): even
// if a compiler check is bypassed, no credential is ever handed out for a
// capability the stage did not declare.
var ErrUndeclaredCapability = errors.New("credentials: capability not declared for this stage")

// ErrNoCredentialForCapability is returned when a declared capability has no
// grant configured — the capability doesn't require a credential, or the
// instance is misconfigured. Callers that require a credential for a
// declared capability should treat this as fail-closed too.
var ErrNoCredentialForCapability = errors.New("credentials: no credential grant configured for capability")

// SecretRegistrar receives every secret value this package resolves so it
// can be scrubbed out of anything written to rest. The run journal's
// RegistryScrubber (issue #8) satisfies this structurally; tests can use a
// no-op or spy.
type SecretRegistrar interface {
	Register(secret []byte)
}

// Grant maps one goober's credential key to the token ref that backs it. Keys
// are canonical capabilities or invocation-internal named MCP keys. Goober is
// empty only for runner-owned deterministic work.
type Grant struct {
	Goober     string
	Capability string
	Ref        string
}

// Injector resolves credentials scoped to one identity and the keys declared
// by a stage or its goober manifest. It never materializes another goober's
// grant, and it registers every value it resolves with its SecretRegistrar
// before handing it back — nothing bypasses the scrubber.
type Injector struct {
	resolver       Resolver
	grants         map[string]string // credential key -> ref name
	credentialKeys []string
	registrar      SecretRegistrar
}

// NewInjector builds an Injector for runner-owned deterministic work. Only
// grants with an empty Goober are in scope.
func NewInjector(resolver Resolver, grants []Grant, registrar SecretRegistrar) (*Injector, error) {
	return newInjector(resolver, "", grants, registrar)
}

// NewGooberInjector builds an Injector scoped to exactly goober. Grants for
// every other goober, and runner-owned grants with an empty Goober, remain
// unreachable even if the stage declares the same capability.
func NewGooberInjector(resolver Resolver, goober string, grants []Grant, registrar SecretRegistrar) (*Injector, error) {
	if goober == "" {
		return nil, errors.New("credentials: goober injector requires a non-empty goober identity")
	}
	return newInjector(resolver, goober, grants, registrar)
}

// NewGooberInjectorWithCredentialKeys builds a goober-scoped Injector that
// always materializes the named non-capability credential keys. The caller must
// derive these keys from that goober's explicit credential declarations.
func NewGooberInjectorWithCredentialKeys(resolver Resolver, goober string, grants []Grant, credentialKeys []string, registrar SecretRegistrar) (*Injector, error) {
	injector, err := NewGooberInjector(resolver, goober, grants, registrar)
	if err != nil {
		return nil, err
	}
	implicit := make([]string, 0, len(credentialKeys))
	implicitSeen := make(map[string]bool, len(credentialKeys))
	for _, key := range credentialKeys {
		if key == "" {
			return nil, errors.New("credentials: implicit credential key must not be empty")
		}
		if implicitSeen[key] {
			continue
		}
		if _, ok := injector.grants[key]; !ok {
			return nil, fmt.Errorf("credentials: implicit credential key %q has no grant for goober %q", key, goober)
		}
		implicitSeen[key] = true
		implicit = append(implicit, key)
	}
	injector.credentialKeys = implicit
	return injector, nil
}

func newInjector(resolver Resolver, goober string, grants []Grant, registrar SecretRegistrar) (*Injector, error) {
	if resolver == nil {
		return nil, errors.New("credentials: injector requires a non-nil resolver")
	}
	if registrar == nil {
		return nil, errors.New("credentials: injector requires a non-nil registrar")
	}
	byCap := make(map[string]string, len(grants))
	type grantKey struct {
		goober     string
		capability string
	}
	seen := make(map[grantKey]bool, len(grants))
	for _, g := range grants {
		if g.Capability == "" || g.Ref == "" {
			return nil, fmt.Errorf("credentials: grant with empty capability or ref: %+v", g)
		}
		key := grantKey{goober: g.Goober, capability: g.Capability}
		if seen[key] {
			return nil, fmt.Errorf("credentials: duplicate grant for goober %q capability %q", g.Goober, g.Capability)
		}
		seen[key] = true
		if g.Goober != goober {
			continue
		}
		byCap[g.Capability] = g.Ref
	}
	return &Injector{resolver: resolver, grants: byCap, registrar: registrar}, nil
}

// Materialize resolves credentials for the stage's declared capabilities plus
// the goober manifest's explicit non-capability credential keys. A capability
// with no configured grant is simply skipped (not every capability is
// credentialed, e.g. "telemetry:read"); resolution failure for any granted key
// fails the whole call closed, so a stage never starts half-credentialed.
func (i *Injector) Materialize(ctx context.Context, declared []string) (*Set, error) {
	keys := make([]string, 0, len(declared)+len(i.credentialKeys))
	seen := make(map[string]bool, cap(keys))
	addKeys := func(values []string) {
		for _, key := range values {
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	addKeys(declared)
	addKeys(i.credentialKeys)
	return i.materialize(ctx, keys)
}

// MaterializeRestricted resolves exactly the credential keys admitted by an
// execution policy. Unlike Materialize, it does not implicitly add goober-level
// credential keys, so a nested child cannot inherit credentials omitted from
// its effective policy.
func (i *Injector) MaterializeRestricted(ctx context.Context, admitted []string) (*Set, error) {
	keys := make([]string, 0, len(admitted))
	seen := make(map[string]bool, len(admitted))
	for _, key := range admitted {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return i.materialize(ctx, keys)
}

func (i *Injector) materialize(ctx context.Context, keys []string) (*Set, error) {
	s := &Set{
		declared: make(map[string]bool, len(keys)),
		tokens:   make(map[string]string, len(keys)),
		expiries: make(map[string]time.Time, len(keys)),
	}
	expiring, _ := i.resolver.(ExpiringResolver)
	for _, key := range keys {
		s.declared[key] = true
		ref, ok := i.grants[key]
		if !ok {
			continue
		}
		var token string
		var expiresAt time.Time
		var err error
		if expiring != nil {
			token, expiresAt, err = expiring.ResolveWithExpiry(ctx, ref)
		} else {
			token, err = i.resolver.Resolve(ctx, ref)
		}
		if err != nil {
			return nil, fmt.Errorf("credentials: materialize credential key %q: %w", key, err)
		}
		i.registrar.Register([]byte(token))
		s.tokens[key] = token
		if !expiresAt.IsZero() {
			s.expiries[key] = expiresAt
		}
	}
	return s, nil
}

// Set is the credential set materialized for one stage's declared credential
// keys. It is the only thing handed to a stage executor or provider — never the
// Injector or Resolver, which can reach every configured ref.
type Set struct {
	declared map[string]bool
	tokens   map[string]string
	// expiries carries each materialized value's stated expiry when the
	// backing source reported one (ExpiringResolver). Absent entries mean the
	// source stated none — a static token whose life is unknowable here.
	expiries map[string]time.Time
}

// Token returns the credential for capability, fail closed: it is an error
// both when capability was never declared for this Set (ErrUndeclaredCapability)
// and when it was declared but has no credential materialized
// (ErrNoCredentialForCapability).
func (s *Set) Token(ctx context.Context, capability string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !s.declared[capability] {
		return "", fmt.Errorf("%w: %q", ErrUndeclaredCapability, capability)
	}
	tok, ok := s.tokens[capability]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoCredentialForCapability, capability)
	}
	return tok, nil
}

// Expiry reports the stated expiry of the credential materialized for
// capability. ok is false when no credential was materialized for it or when
// the backing source stated no expiry (DS10: only sources that know a value's
// life report one; nothing is invented).
func (s *Set) Expiry(capability string) (time.Time, bool) {
	expiresAt, ok := s.expiries[capability]
	return expiresAt, ok
}

// For returns a capability-scoped token source: a value whose Token(ctx)
// (string, error) method resolves through this Set. It satisfies any
// TokenSource-shaped interface structurally (e.g. providers.TokenSource),
// without this package importing providers.
func (s *Set) For(capability string) *ScopedTokenSource {
	return &ScopedTokenSource{set: s, capability: capability}
}

// ScopedTokenSource is a Set narrowed to one capability. Its Token method
// signature — Token(context.Context) (string, error) — matches
// providers.TokenSource and any equivalently-shaped interface.
type ScopedTokenSource struct {
	set        *Set
	capability string
}

// Token resolves the credential for the bound capability.
func (t *ScopedTokenSource) Token(ctx context.Context) (string, error) {
	return t.set.Token(ctx, t.capability)
}
