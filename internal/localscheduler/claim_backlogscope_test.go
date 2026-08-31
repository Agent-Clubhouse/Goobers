package localscheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This file covers personal-gaggle-routing §5.3: backlog-scoped claim identity
// and its compatibility/migration path. The property under test throughout is
// that OWNERSHIP follows the backlog container, not the gaggle — two gaggles
// sharing one physical backlog contend, and equal external IDs in different
// backlogs do not.

func githubBacklog(t *testing.T, project string) apiv1.BacklogIdentity {
	t.Helper()
	id, err := apiv1.BacklogIdentityFromRef(apiv1.BacklogRef{
		Provider: apiv1.ProviderGitHub,
		Project:  project,
	})
	if err != nil {
		t.Fatalf("backlog identity for %q: %v", project, err)
	}
	return id
}

func newTestLedger(t *testing.T) (*ClaimLedger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	return ledger, path
}

// TestBacklogScopedClaimIsExclusiveAcrossGaggles is the core guarantee: the
// router gaggle and a destination gaggle drawing from ONE private backlog
// cannot both hold the same item, even though each has its own gaggle name.
func TestBacklogScopedClaimIsExclusiveAcrossGaggles(t *testing.T) {
	ledger, _ := newTestLedger(t)
	backlog := githubBacklog(t, "gim-home/brandiv.goobers")

	ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: backlog,
	}, "run-router", "routing", time.Hour)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}

	ok, holder, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: backlog,
	}, "run-personal", "implementation", time.Hour)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("a second gaggle claimed an item already held in the same backlog")
	}
	if holder != "run-router" {
		t.Fatalf("holder = %q, want %q", holder, "run-router")
	}
}

// TestEqualExternalIDsInDifferentBacklogsAreIndependent is the necessary
// converse: scoping must not create false contention between unrelated
// backlogs that happen to number their items the same way.
func TestEqualExternalIDsInDifferentBacklogsAreIndependent(t *testing.T) {
	ledger, _ := newTestLedger(t)
	private := githubBacklog(t, "gim-home/brandiv.goobers")
	team := githubBacklog(t, "gim-home/dev-brandiv")

	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: private,
	}, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim in private backlog: ok=%v err=%v", ok, err)
	}
	ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "team", Provider: "github", ExternalID: "42", Backlog: team,
	}, "run-b", "implementation", time.Hour)
	if err != nil {
		t.Fatalf("claim in team backlog: %v", err)
	}
	if !ok {
		t.Fatal("item 42 in a DIFFERENT backlog was refused; backlogs must not contend")
	}
}

// TestBacklogScopedClaimIsIdempotentForSameRun keeps a retried stage attempt
// from being refused by its own earlier claim.
func TestBacklogScopedClaimIsIdempotentForSameRun(t *testing.T) {
	ledger, _ := newTestLedger(t)
	key := ClaimKey{Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: githubBacklog(t, "o/r")}

	if ok, _, err := ledger.ClaimScoped(key, "run-1", "routing", time.Hour); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.ClaimScoped(key, "run-1", "routing", 2*time.Hour); err != nil || !ok {
		t.Fatalf("re-claim by same run: ok=%v err=%v", ok, err)
	}
	entry, held := ledger.LookupScoped(key)
	if !held {
		t.Fatal("entry missing after renewal")
	}
	if entry.Backlog == nil {
		t.Fatal("renewed entry lost its backlog identity")
	}
}

// TestReleaseAndRenewEntryUseBacklogScope proves the entry-shaped helpers every
// recovery/close-out path uses reconstruct the v3 key rather than falling back
// to the gaggle-scoped one (which would silently no-op).
func TestReleaseAndRenewEntryUseBacklogScope(t *testing.T) {
	ledger, _ := newTestLedger(t)
	key := ClaimKey{Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: githubBacklog(t, "o/r")}
	if ok, _, err := ledger.ClaimScoped(key, "run-1", "routing", time.Hour); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)

	renewed, err := ledger.RenewEntry(entry, 2*time.Hour)
	if err != nil || !renewed {
		t.Fatalf("RenewEntry: ok=%v err=%v", renewed, err)
	}
	if err := ledger.ReleaseEntry(entry, "run-1"); err != nil {
		t.Fatalf("ReleaseEntry: %v", err)
	}
	if _, held := ledger.LookupScoped(key); held {
		t.Fatal("ReleaseEntry did not release the backlog-scoped claim")
	}
}

func TestForceReleaseEntryUsesBacklogScope(t *testing.T) {
	ledger, _ := newTestLedger(t)
	key := ClaimKey{Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: githubBacklog(t, "o/r")}
	if ok, _, err := ledger.ClaimScoped(key, "run-1", "routing", time.Hour); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	entry, _ := ledger.LookupScoped(key)
	if err := ledger.ForceReleaseEntry(entry, "operator"); err != nil {
		t.Fatalf("ForceReleaseEntry: %v", err)
	}
	if _, held := ledger.LookupScoped(key); held {
		t.Fatal("ForceReleaseEntry did not release the backlog-scoped claim")
	}
}

func TestReclaimAllPreservesBacklogScope(t *testing.T) {
	ledger, _ := newTestLedger(t)
	backlog := githubBacklog(t, "o/r")
	entries := []ClaimEntry{{
		ItemID: "42", ExternalID: "42", Gaggle: "router", Provider: "github",
		Backlog: &backlog, RunID: "old-run", Workflow: "routing",
	}}
	if ok, _, err := ledger.ReclaimAll(entries, "new-run", "routing", time.Hour); err != nil || !ok {
		t.Fatalf("ReclaimAll: ok=%v err=%v", ok, err)
	}
	entry, held := ledger.LookupScoped(ClaimKey{
		Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: backlog,
	})
	if !held {
		t.Fatal("reclaimed entry is not under the backlog-scoped key")
	}
	if entry.RunID != "new-run" {
		t.Fatalf("RunID = %q, want %q", entry.RunID, "new-run")
	}
}

// --- Compatibility and migration (§5.3) ---

// TestOpenClaimLedgerReadsLegacySchemas keeps a pre-backlog-scoping ledger
// loadable: an in-place upgrade must not require operators to discard leases.
func TestOpenClaimLedgerReadsLegacySchemas(t *testing.T) {
	for _, schema := range []string{claimLedgerSchema, claimLedgerSchemaBacklogScoped} {
		path := filepath.Join(t.TempDir(), "claims.json")
		state := claimLedgerState{Schema: schema, Entries: map[string]ClaimEntry{}}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenClaimLedger(path); err != nil {
			t.Fatalf("schema %q should load: %v", schema, err)
		}
	}
}

// TestLedgerKeepsLegacySchemaUntilBacklogScopeAppears is the other half of
// compatibility: an instance that never routes keeps writing the old schema
// string, so an older binary can still read its ledger. Only once a v3 key
// exists does the schema bump and lock older binaries out (rather than letting
// them misread a backlog-scoped lease as gaggle-scoped).
func TestLedgerKeepsLegacySchemaUntilBacklogScopeAppears(t *testing.T) {
	ledger, path := newTestLedger(t)
	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "team", Provider: "github", ExternalID: "7",
	}, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("gaggle-scoped claim: ok=%v err=%v", ok, err)
	}
	if got := readLedgerSchema(t, path); got != claimLedgerSchema {
		t.Fatalf("schema = %q, want %q for a gaggle-scoped-only ledger", got, claimLedgerSchema)
	}

	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "8", Backlog: githubBacklog(t, "o/r"),
	}, "run-2", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("backlog-scoped claim: ok=%v err=%v", ok, err)
	}
	if got := readLedgerSchema(t, path); got != claimLedgerSchemaBacklogScoped {
		t.Fatalf("schema = %q, want %q once a backlog-scoped entry exists", got, claimLedgerSchemaBacklogScoped)
	}
}

func readLedgerSchema(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var state claimLedgerState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse ledger: %v", err)
	}
	return state.Schema
}

// TestMigrateBacklogScopePromotesGaggleScopedClaims is the upgrade path: a
// gaggle-scoped lease taken by the previous binary becomes authoritative under
// the backlog key, and is then exclusive against a sibling gaggle.
func TestMigrateBacklogScopePromotesGaggleScopedClaims(t *testing.T) {
	ledger, _ := newTestLedger(t)
	backlog := githubBacklog(t, "gim-home/brandiv.goobers")
	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "42",
	}, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed v2 claim: ok=%v err=%v", ok, err)
	}

	if err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return backlog, nil
	}); err != nil {
		t.Fatalf("MigrateBacklogScope: %v", err)
	}

	entry, held := ledger.LookupScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: backlog,
	})
	if !held {
		t.Fatal("migrated claim is not under the backlog-scoped key")
	}
	if entry.RunID != "run-1" {
		t.Fatalf("RunID = %q, want run-1", entry.RunID)
	}
	if _, stillLegacy := ledger.LookupScoped(ClaimKey{
		Gaggle: "personal", Provider: "github", ExternalID: "42",
	}); stillLegacy {
		t.Fatal("the old gaggle-scoped key was left behind")
	}

	// The whole point of migrating: a sibling gaggle must now be refused.
	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "router", Provider: "github", ExternalID: "42", Backlog: backlog,
	}, "run-2", "routing", time.Hour); err != nil || ok {
		t.Fatalf("sibling gaggle claim after migration: ok=%v err=%v (want refused)", ok, err)
	}
}

// TestMigrateBacklogScopeSkipsNonBacklogClaims protects pull-request leases,
// whose exclusivity is legitimately gaggle-scoped, from being rescoped.
func TestMigrateBacklogScopeSkipsNonBacklogClaims(t *testing.T) {
	ledger, _ := newTestLedger(t)
	prKey := ClaimKey{Gaggle: "team", Provider: "github", ExternalID: "pr/17"}
	if ok, _, err := ledger.ClaimScoped(prKey, "run-1", "merge-review", time.Hour); err != nil || !ok {
		t.Fatalf("seed pr claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return apiv1.BacklogIdentity{}, ErrClaimNotBacklogScoped
	}); err != nil {
		t.Fatalf("MigrateBacklogScope: %v", err)
	}
	entry, held := ledger.LookupScoped(prKey)
	if !held {
		t.Fatal("pull-request claim was moved off its gaggle-scoped key")
	}
	if entry.Backlog != nil {
		t.Fatal("pull-request claim was given a backlog identity")
	}
}

// TestMigrateBacklogScopeRetainsUnresolvedClaims is the conservative branch:
// an entry whose owning gaggle can no longer be resolved keeps its live lease
// rather than being freed (double-claim) or guessed into a backlog (collision).
func TestMigrateBacklogScopeRetainsUnresolvedClaims(t *testing.T) {
	ledger, _ := newTestLedger(t)
	key := ClaimKey{Gaggle: "removed-gaggle", Provider: "github", ExternalID: "42"}
	if ok, _, err := ledger.ClaimScoped(key, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return apiv1.BacklogIdentity{}, ErrLegacyClaimOwnershipUnresolved
	}); err != nil {
		t.Fatalf("MigrateBacklogScope should tolerate unresolved entries: %v", err)
	}
	if _, held := ledger.LookupScoped(key); !held {
		t.Fatal("an unresolved live claim was dropped instead of retained")
	}
	// And it still blocks a backlog-scoped claimant for the same item, since
	// the unresolved entry might belong to that very backlog.
	if ok, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle: "removed-gaggle", Provider: "github", ExternalID: "42", Backlog: githubBacklog(t, "o/r"),
	}, "run-2", "implementation", time.Hour); err != nil || ok {
		t.Fatalf("claim over an unresolved lease: ok=%v err=%v (want refused)", ok, err)
	}
}

// TestMigrateBacklogScopeDetectsLiveCollision refuses to silently discard one
// of two live leases that collapse onto a single backlog-scoped key — that is
// precisely the double-claim this feature exists to make impossible, so it must
// surface rather than be resolved by coin flip.
func TestMigrateBacklogScopeDetectsLiveCollision(t *testing.T) {
	ledger, _ := newTestLedger(t)
	for _, gaggle := range []string{"personal-a", "personal-b"} {
		if ok, _, err := ledger.ClaimScoped(ClaimKey{
			Gaggle: gaggle, Provider: "github", ExternalID: "42",
		}, "run-"+gaggle, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("seed %s: ok=%v err=%v", gaggle, ok, err)
		}
	}
	backlog := githubBacklog(t, "gim-home/brandiv.goobers")
	err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return backlog, nil
	})
	if err == nil {
		t.Fatal("two live leases collapsing onto one backlog key must be reported")
	}

	// The ledger must be untouched after a refused migration.
	for _, gaggle := range []string{"personal-a", "personal-b"} {
		if _, held := ledger.LookupScoped(ClaimKey{
			Gaggle: gaggle, Provider: "github", ExternalID: "42",
		}); !held {
			t.Fatalf("%s lost its claim after a refused migration", gaggle)
		}
	}
}

// TestMigrateBacklogScopeIsIdempotent keeps repeated daemon restarts cheap and
// safe: an already-v3 ledger is left exactly as it is.
func TestMigrateBacklogScopeIsIdempotent(t *testing.T) {
	ledger, _ := newTestLedger(t)
	backlog := githubBacklog(t, "o/r")
	key := ClaimKey{Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: backlog}
	if ok, _, err := ledger.ClaimScoped(key, "run-1", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	calls := 0
	resolve := func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		calls++
		return backlog, nil
	}
	for range 2 {
		if err := ledger.MigrateBacklogScope(resolve); err != nil {
			t.Fatalf("MigrateBacklogScope: %v", err)
		}
	}
	if calls != 0 {
		t.Fatalf("resolver called %d times for an already-v3 entry, want 0", calls)
	}
	if _, held := ledger.LookupScoped(key); !held {
		t.Fatal("claim lost across repeated migrations")
	}
}

// TestBacklogScopedKeyRejectsIncompleteIdentity fails closed rather than
// producing a degenerate key that could collide across backlogs.
func TestBacklogScopedKeyRejectsIncompleteIdentity(t *testing.T) {
	ledger, _ := newTestLedger(t)
	_, _, err := ledger.ClaimScoped(ClaimKey{
		Gaggle:     "personal",
		Provider:   "github",
		ExternalID: "42",
		Backlog:    apiv1.BacklogIdentity{Provider: apiv1.ProviderGitHub, Owner: "only-owner"},
	}, "run-1", "implementation", time.Hour)
	if err == nil {
		t.Fatal("an incomplete backlog identity must not produce a claim key")
	}
}

// TestMigrateBacklogScopePreservesLiveV3AgainstExpiredLegacy is the narrow
// race the existing-v3 collision branch used to lose: a v3 lease was taken
// (post-upgrade) while a pre-v3 entry for the SAME item was still sitting in
// the ledger, expired but not yet reaped. Migration resolved that stale entry
// onto the already-occupied v3 key, and — having computed the collision winner
// only to discard it — planned the rewrite anyway, replacing the live owner
// with an expired lease. The item then read as free and the next claimant took
// it while run-live still believed it held it: exactly the double-claim
// backlog scoping exists to prevent.
func TestMigrateBacklogScopePreservesLiveV3AgainstExpiredLegacy(t *testing.T) {
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	backlog := githubBacklog(t, "gim-home/brandiv.goobers")
	legacyKey := ClaimKey{Gaggle: "personal", Provider: "github", ExternalID: "42"}
	v3Key := ClaimKey{Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: backlog}

	if ok, _, err := ledger.ClaimScoped(legacyKey, "run-legacy", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed pre-v3 claim: ok=%v err=%v", ok, err)
	}
	now = now.Add(10 * time.Minute) // the pre-v3 lease is now expired but unreaped
	if ok, _, err := ledger.ClaimScoped(v3Key, "run-live", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live v3 claim: ok=%v err=%v", ok, err)
	}

	if err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return backlog, nil
	}); err != nil {
		t.Fatalf("MigrateBacklogScope: %v", err)
	}

	entry, held := ledger.LookupScoped(v3Key)
	if !held || entry.RunID != "run-live" {
		t.Fatalf("v3 entry = (%+v, %v), want the live lease held by run-live", entry, held)
	}
	if entry.expired(now) {
		t.Fatalf("live v3 lease was replaced by an expired one: %+v", entry)
	}
	// The surviving owner must still be exclusive against a fresh claimant.
	if ok, holder, err := ledger.ClaimScoped(v3Key, "run-other", "implementation", time.Hour); err != nil || ok || holder != "run-live" {
		t.Fatalf("competing claim = (ok=%v holder=%q err=%v), want refused with run-live holding", ok, holder, err)
	}

	// The rewrite must also survive a reopen — an in-memory-only preservation
	// would still lose the owner on the next daemon start.
	reopened, err := OpenClaimLedger(path, WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("reopen ledger: %v", err)
	}
	if entry, held := reopened.LookupScoped(v3Key); !held || entry.RunID != "run-live" {
		t.Fatalf("persisted v3 entry = (%+v, %v), want run-live", entry, held)
	}
}

// TestMigrateBacklogScopeReplacesExpiredV3WithLiveLegacy is the converse the
// fix must not break: when the entry already at the v3 key is the expired one,
// the live pre-v3 lease still wins and is promoted, so its owner keeps the
// item under the authoritative key.
func TestMigrateBacklogScopeReplacesExpiredV3WithLiveLegacy(t *testing.T) {
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	ledger, err := OpenClaimLedger(filepath.Join(t.TempDir(), "claims.json"),
		WithLedgerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	backlog := githubBacklog(t, "gim-home/brandiv.goobers")
	legacyKey := ClaimKey{Gaggle: "personal", Provider: "github", ExternalID: "42"}
	v3Key := ClaimKey{Gaggle: "personal", Provider: "github", ExternalID: "42", Backlog: backlog}

	if ok, _, err := ledger.ClaimScoped(v3Key, "run-stale", "implementation", time.Minute); err != nil || !ok {
		t.Fatalf("seed v3 claim: ok=%v err=%v", ok, err)
	}
	now = now.Add(10 * time.Minute) // the v3 lease is now expired but unreaped
	if ok, _, err := ledger.ClaimScoped(legacyKey, "run-live", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed live pre-v3 claim: ok=%v err=%v", ok, err)
	}

	if err := ledger.MigrateBacklogScope(func(ClaimEntry) (apiv1.BacklogIdentity, error) {
		return backlog, nil
	}); err != nil {
		t.Fatalf("MigrateBacklogScope: %v", err)
	}
	entry, held := ledger.LookupScoped(v3Key)
	if !held || entry.RunID != "run-live" {
		t.Fatalf("v3 entry = (%+v, %v), want the live lease promoted from pre-v3 scope", entry, held)
	}
	if _, held := ledger.LookupScoped(legacyKey); held {
		t.Fatal("the promoted lease was left behind under its pre-v3 key")
	}
}
