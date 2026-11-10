package lease

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestApplyExpiryMapsReleasedNegativeInfinityToNoExpiry(t *testing.T) {
	current := Current{Holder: "", Epoch: 2}
	applyExpiry(&current, pgtype.Timestamptz{
		Valid: true, InfinityModifier: pgtype.NegativeInfinity,
	}, true)
	if current.ExpiresAt != nil || !current.Expired {
		t.Fatalf("released lease expiry = %+v", current)
	}
}
