package credentials

import (
	"context"
	"errors"
	"fmt"
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

// Grant maps a capability name (e.g. "github:issues:write", as declared on a
// Goober/Task — docs/requirements/goober.md GBO-052) to the token ref that
// backs it.
type Grant struct {
	Capability string
	Ref        string
}

// Injector resolves credentials scoped to a stage's declared capabilities.
// It never materializes a credential for a capability that was not declared,
// and it registers every value it resolves with its SecretRegistrar before
// handing it back — nothing bypasses the scrubber.
type Injector struct {
	resolver  *Resolver
	grants    map[string]string // capability -> ref name
	registrar SecretRegistrar
}

// NewInjector builds an Injector over resolver, scoped by grants, registering
// every resolved secret with registrar. registrar must not be nil; pass a
// no-op implementation in tests that don't care about redaction.
func NewInjector(resolver *Resolver, grants []Grant, registrar SecretRegistrar) (*Injector, error) {
	if resolver == nil {
		return nil, errors.New("credentials: injector requires a non-nil resolver")
	}
	if registrar == nil {
		return nil, errors.New("credentials: injector requires a non-nil registrar")
	}
	byCap := make(map[string]string, len(grants))
	for _, g := range grants {
		if g.Capability == "" || g.Ref == "" {
			return nil, fmt.Errorf("credentials: grant with empty capability or ref: %+v", g)
		}
		if _, dup := byCap[g.Capability]; dup {
			return nil, fmt.Errorf("credentials: duplicate grant for capability %q", g.Capability)
		}
		byCap[g.Capability] = g.Ref
	}
	return &Injector{resolver: resolver, grants: byCap, registrar: registrar}, nil
}

// Materialize resolves credentials for exactly the given declared
// capabilities — the stage's own declaration, already admitted by the DSL
// compiler (SEC-042) — and nothing else. A capability with no configured
// grant is simply skipped (not every capability is credentialed, e.g.
// "telemetry:read"); resolution failure for a capability that IS granted
// fails the whole call closed, so a stage never starts half-credentialed.
func (i *Injector) Materialize(ctx context.Context, declared []string) (*Set, error) {
	s := &Set{
		declared: make(map[string]bool, len(declared)),
		tokens:   make(map[string]string, len(declared)),
	}
	for _, capability := range declared {
		s.declared[capability] = true
		ref, ok := i.grants[capability]
		if !ok {
			continue
		}
		token, err := i.resolver.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("credentials: materialize capability %q: %w", capability, err)
		}
		i.registrar.Register([]byte(token))
		s.tokens[capability] = token
	}
	return s, nil
}

// Set is the credential set materialized for one stage's declared
// capabilities. It is the only thing handed to a stage executor or provider
// — never the Injector or Resolver, which can reach every configured ref.
type Set struct {
	declared map[string]bool
	tokens   map[string]string
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
