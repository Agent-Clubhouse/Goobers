package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/platform/secfile"
)

// ErrTokenRefNotFound is returned when no TokenRef is registered under the
// requested name.
var ErrTokenRefNotFound = errors.New("credentials: token ref not found")

// ErrTokenRefEmpty is returned when a TokenRef resolves to an empty value —
// treated as misconfiguration, not a valid (empty) secret.
var ErrTokenRefEmpty = errors.New("credentials: token ref resolved to an empty value")

// TokenRef names one local-tier secret source: exactly one of Env or File
// must be set. It is the tiers 1-2 shape of instance.yaml's token refs
// (docs/ARCHITECTURE.md §6); a Key Vault ref is the tier-3 counterpart behind
// the same seam.
type TokenRef struct {
	// Name is the logical name other config (capability grants) refers to
	// this ref by, e.g. "github-issues".
	Name string
	// Env is the environment variable holding the secret value.
	Env string
	// File is the path to a file whose (trimmed) contents are the secret
	// value.
	File string
}

func (r TokenRef) validate() error {
	if r.Name == "" {
		return errors.New("credentials: token ref has no name")
	}
	if (r.Env == "") == (r.File == "") {
		return fmt.Errorf("credentials: token ref %q must set exactly one of Env or File", r.Name)
	}
	return nil
}

// resolve reads the secret value for this ref from the process environment
// or filesystem.
func (r TokenRef) resolve() (string, error) {
	var raw string
	var ok bool
	switch {
	case r.Env != "":
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
	}
	val := strings.TrimSpace(raw)
	if val == "" {
		return "", fmt.Errorf("%w: ref %q", ErrTokenRefEmpty, r.Name)
	}
	return val, nil
}

// Resolver resolves a named secret reference. Implementations must honor
// context cancellation. The local implementation reads env vars and files;
// Azure Key Vault supplies the tier-3 implementation behind this same seam
// (SEC-010), without changes to credential consumers.
type Resolver interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// ResolveFunc is a dynamic secret source behind the Resolver seam: a minting
// credential source (e.g. GitHub App installation tokens, #686) that produces
// a value per resolve instead of re-reading an env var or file. Implementations
// own their caching and must honor context cancellation.
type ResolveFunc func(ctx context.Context) (string, error)

// envFileResolver holds no secret material itself. Every TokenRef is re-read
// at resolve time so a rotated env var or file takes effect without restarting
// the process; dynamic sources are consulted per resolve for the same reason.
type envFileResolver struct {
	refs    map[string]TokenRef
	sources map[string]ResolveFunc
}

var _ Resolver = (*envFileResolver)(nil)

// NewResolver builds the local env/file Resolver from a set of token refs.
// Names must be unique and each ref must be well-formed (exactly one of
// Env/File set).
func NewResolver(refs []TokenRef) (Resolver, error) {
	return NewResolverWithSources(refs, nil)
}

// NewResolverWithSources builds the local Resolver from token refs plus named
// dynamic sources (ResolveFunc). Refs and sources share one namespace — a
// consumer resolves by name without knowing whether the value is read or
// minted — so a name may not appear in both.
func NewResolverWithSources(refs []TokenRef, sources map[string]ResolveFunc) (Resolver, error) {
	byName := make(map[string]TokenRef, len(refs))
	for _, r := range refs {
		if err := r.validate(); err != nil {
			return nil, err
		}
		if _, dup := byName[r.Name]; dup {
			return nil, fmt.Errorf("credentials: duplicate token ref name %q", r.Name)
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
	return &envFileResolver{refs: byName, sources: sources}, nil
}

// Resolve returns the secret value for the named token ref. ctx is accepted
// for future resolvers that need it (e.g. a Key Vault client at V2); the
// local env/file resolver is synchronous and ignores it beyond cancellation.
func (r *envFileResolver) Resolve(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if fn, ok := r.sources[name]; ok {
		value, err := fn(ctx)
		if err != nil {
			return "", fmt.Errorf("credentials: token ref %q: %w", name, err)
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%w: ref %q", ErrTokenRefEmpty, name)
		}
		return value, nil
	}
	ref, ok := r.refs[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrTokenRefNotFound, name)
	}
	return ref.resolve()
}
