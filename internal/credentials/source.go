package credentials

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
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
		if fi, statErr := os.Stat(r.File); statErr == nil {
			if w := insecureTokenFileWarning(r.Name, r.File, fi.Mode()); w != "" {
				fmt.Fprintln(os.Stderr, w)
			}
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

// insecureTokenFileWarning returns a warning message when a token file is
// readable or writable by group or other — a secret file should be owner-only
// (e.g. 0600). It returns "" when the permissions are safe. Kept pure (mode
// passed in) so it is unit-testable; resolve prints the result to stderr.
func insecureTokenFileWarning(name, path string, mode fs.FileMode) string {
	if mode.Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("credentials: warning: token file %q for ref %q is accessible to group/other "+
		"(mode %#o); tighten it to 0600", path, name, mode.Perm())
}

// Resolver resolves named token refs to secret values. It holds no secret
// material itself — every TokenRef is re-read at resolve time so a rotated
// env var or file takes effect without restarting the process.
type Resolver struct {
	refs map[string]TokenRef
}

// NewResolver builds a Resolver from a set of token refs. Names must be
// unique and each ref must be well-formed (exactly one of Env/File set).
func NewResolver(refs []TokenRef) (*Resolver, error) {
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
	return &Resolver{refs: byName}, nil
}

// Resolve returns the secret value for the named token ref. ctx is accepted
// for future resolvers that need it (e.g. a Key Vault client at V2); the
// local env/file resolver is synchronous and ignores it beyond cancellation.
func (r *Resolver) Resolve(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref, ok := r.refs[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrTokenRefNotFound, name)
	}
	return ref.resolve()
}
