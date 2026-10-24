//go:build cloudintegration

package podauth

import (
	"errors"
	"testing"
	"time"

	shared "github.com/goobers/goobers/internal/podauth"
)

func TestPrunedRevocationNeverReadmitsUnderReplicaSkew(t *testing.T) {
	pool := revocationPool(t)
	var dbNow time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	apiNow := dbNow.Add(-60 * time.Second) // API replica clock 60s behind the DB
	a := authFixture(t, &testRevocations{})
	a.GrantKeys.WithClock(func() time.Time { return apiNow })
	a.revocations = &Revocations{Pool: pool, InstanceID: "instance"}
	token, grant, err := a.GrantKeys.MintCredentialGrant(shared.CredentialGrant{RunID: "run", Stage: "stage", Capabilities: []string{"repo:push"}}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// grant.ExpiresAt = dbNow-30s: expired per DB, valid for 30s more per the API replica.
	if err := a.revocations.(*Revocations).Revoke(t.Context(), grant.ID, grant.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyCredentialGrant(t.Context(), token); !errors.Is(err, shared.ErrInvalidCredentialGrant) {
		t.Fatalf("revoked grant before prune: %v", err)
	}
	manager, owner := revocationOwner(t, pool, "controller")
	if err := PruneExpired(t.Context(), manager, owner); err != nil {
		t.Fatal(err)
	}
	if revoked, err := a.revocations.IsRevoked(t.Context(), grant.ID); err != nil || revoked {
		t.Fatalf("expired revocation was not pruned: revoked=%v err=%v", revoked, err)
	}
	_, err = a.VerifyCredentialGrant(t.Context(), token)
	t.Logf("grant expires %s (DB now %s, API now %s); after prune: err=%v", grant.ExpiresAt.Format(time.RFC3339), dbNow.Format(time.RFC3339), apiNow.Format(time.RFC3339), err)
	if !errors.Is(err, shared.ErrExpiredCredentialGrant) {
		t.Fatalf("pruned grant must be DB-expired despite API skew: %v", err)
	}
}
