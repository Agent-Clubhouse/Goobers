package planes

import (
	"context"
	"time"
)

// GrantRevocations supplies shared credential-refresh grant revocation. Callers
// must fail closed on read errors. Implementations must bound replica staleness;
// the cloud store reads the primary for every admission. IDs are per-mint nonces,
// never the run/stage/attempt triple. Expiry is retention metadata, not permission
// to erase a revocation while a verifier could still accept the grant.
type GrantRevocations interface {
	Revoke(context.Context, string, time.Time) error
	IsRevoked(context.Context, string) (bool, error)
}
