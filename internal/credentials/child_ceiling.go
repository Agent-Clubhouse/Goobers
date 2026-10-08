package credentials

import (
	"context"
	"errors"
	"slices"

	"github.com/goobers/goobers/internal/capability"
)

// ErrChildAuthenticationIsolation refuses uncontained HOME helpers, inherited
// auth and raw-token paths. Filtering capability labels cannot attenuate a PAT.
var ErrChildAuthenticationIsolation = errors.New("credentials: child execution requires verified authentication isolation; local HOME, helper and inherited credential paths are unsupported")

// ChildCeiling is host-owned delegation, never read from task inputs. Without
// publication permission only an explicitly delegated model credential may be
// materialized. Repository/provider tokens and named MCP credentials are denied
// even when their capability label says read: their real token scopes are opaque.
type ChildCeiling struct {
	Version          int      `json:"version"`
	AllowPublication bool     `json:"allowPublication"`
	AllowedKeys      []string `json:"allowedKeys"`
}

type childCeilingKey struct{}

// NewChildCeiling intersects the child's ceiling with the enclosing stage.
func NewChildCeiling(publication bool, parent, child []string) ChildCeiling {
	out := ChildCeiling{Version: 1, AllowPublication: publication, AllowedKeys: []string{}}
	for _, key := range child {
		if !capability.Known(key) || !slices.Contains(parent, key) || (!publication && key != string(capability.AgentModel)) {
			continue
		}
		out.AllowedKeys = append(out.AllowedKeys, key)
	}
	slices.Sort(out.AllowedKeys)
	out.AllowedKeys = slices.Compact(out.AllowedKeys)
	return out
}

// Validate refuses widened or noncanonical stored delegation.
func (c ChildCeiling) Validate() error {
	if c.Version != 1 || len(c.AllowedKeys) > 256 || !slices.IsSorted(c.AllowedKeys) {
		return errors.New("credentials: invalid child credential ceiling")
	}
	for i, key := range c.AllowedKeys {
		if !capability.Known(key) || (i > 0 && c.AllowedKeys[i-1] == key) || (!c.AllowPublication && key != string(capability.AgentModel)) {
			return errors.New("credentials: child ceiling exceeds its publication authority")
		}
	}
	return nil
}

// WithChildCeiling copies a trusted ceiling onto one invocation's context.
func WithChildCeiling(ctx context.Context, ceiling ChildCeiling) (context.Context, error) {
	if err := ceiling.Validate(); err != nil {
		return nil, err
	}
	ceiling.AllowedKeys = slices.Clone(ceiling.AllowedKeys)
	return context.WithValue(ctx, childCeilingKey{}, ceiling), nil
}

// FilterChildCredentialKeys applies the same ceiling to daemon credential responses.
func FilterChildCredentialKeys(ctx context.Context, keys []string) []string {
	ceiling, child := ctx.Value(childCeilingKey{}).(ChildCeiling)
	if !child {
		return keys
	}
	allowed := make([]string, 0, len(keys))
	for _, key := range keys {
		if slices.Contains(ceiling.AllowedKeys, key) {
			allowed = append(allowed, key)
		}
	}
	return allowed
}

// RefuseUnisolatedChildProcess is called before local environment construction,
// token materialization or process launch. The current self executor has no
// credential-isolated filesystem/HOME contract, including stored model logins.
// A future isolated backend must provide a distinct validated launch path.
func RefuseUnisolatedChildProcess(ctx context.Context) error {
	if _, child := ctx.Value(childCeilingKey{}).(ChildCeiling); child {
		return ErrChildAuthenticationIsolation
	}
	return nil
}
