//go:build cloudintegration

package claims

import (
	"context"
	"errors"
	"fmt"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/claimrelease"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestSchedulerPolicyStoreSuite(t *testing.T) {
	t.Run("pg-claims-state", func(t *testing.T) {
		store, _ := testStore(t, "controller-a")
		key := claimKey("42")
		ok, holder, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour)
		if err != nil || !ok || holder != "run-a" {
			t.Fatalf("claim = %v %q %v", ok, holder, err)
		}
		id := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "scheduled"}
		at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		if err := store.WriteScheduleDemand("", map[localscheduler.WorkflowIdentity]bool{id: true}); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{id: at}); err != nil {
			t.Fatal(err)
		}
		if demand, err := store.ReadScheduleDemand(""); err != nil || !demand[id] {
			t.Fatalf("demand = %+v, %v", demand, err)
		}
		if evaluations, err := store.ReadTriggerEvaluations(""); err != nil || !evaluations[id].Equal(at) {
			t.Fatalf("evaluations = %+v, %v", evaluations, err)
		}
		if err := store.ReleaseScoped(key, "run-a", claimrelease.Completed.Reason()); err != nil {
			t.Fatal(err)
		}
		if history := store.HistorySnapshot(); len(history) != 1 || history[0].RunID != "run-a" {
			t.Fatalf("history = %+v", history)
		}
	})
}

func TestConcurrentAcquireOneWinner(t *testing.T) {
	store, pool := testStore(t, "controller")
	const contenders = 16
	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, contenders)
	for index := range contenders {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ok, _, err := store.ClaimScoped(
				claimKey("concurrent"), fmt.Sprintf("run-%d", index), "implementation", time.Minute,
			)
			if ok {
				winners.Add(1)
			}
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners.Load() != 1 {
		t.Fatalf("claim winners = %d, want 1", winners.Load())
	}
	var count int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.claim
		WHERE instance_id='test-instance' AND external_id='concurrent'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("claim rows = %d, want 1", count)
	}
}

func TestClaimRetryResolvesBeforeCapacity(t *testing.T) {
	store, pool := testStore(t, "controller")
	key := claimKey("same")
	if ok, _, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("initial claim = %v, %v", ok, err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.claim
			(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
			 claimed_at,renewed_at,expires_at)
		SELECT 'test-instance','g','github','fill-'||n,'fill-run-'||n,'implementation',
		       'token-'||n,$1,clock_timestamp(),clock_timestamp(),clock_timestamp()+interval '1 hour'
		FROM generate_series(1,$2) AS n`,
		store.handle.Epoch, MaxClaims-1,
	); err != nil {
		t.Fatal(err)
	}
	if ok, holder, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok || holder != "run-a" {
		t.Fatalf("exact retry = %v %q %v", ok, holder, err)
	}
	if _, _, err := store.ClaimScoped(key, "run-a", "different", time.Hour); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched retry = %v, want ErrConflict", err)
	}
	if _, _, err := store.ClaimScoped(claimKey("overflow"), "run-z", "implementation", time.Hour); !errors.Is(err, ErrCapacity) {
		t.Fatalf("new key at capacity = %v, want ErrCapacity", err)
	}
}

func TestStaleOwnerRejectedAfterLeaseTakeover(t *testing.T) {
	old, pool := testStore(t, "old")
	if ok, _, err := old.ClaimScoped(claimKey("owned"), "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool, "new")
	if ok, _, err := current.ClaimScoped(claimKey("new"), "run-b", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, _, err := old.ClaimScoped(claimKey("stale"), "run-old", "implementation", time.Hour); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale claim = %v, want ErrStale", err)
	}
	if err := old.WriteScheduleDemand("", map[localscheduler.WorkflowIdentity]bool{
		{Gaggle: "g", Workflow: "w"}: true,
	}); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale state write = %v, want ErrStale", err)
	}
}

func TestFairnessLeaseSameTxAsClaim(t *testing.T) {
	store, pool := testStore(t, "controller")
	key := claimKey("fairness")
	if ok, _, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var claimXID, stateXID string
	if err := pool.QueryRow(t.Context(), `
		SELECT claim.xmin::text,state.xmin::text
		FROM control.claim AS claim
		JOIN control.scheduler_state AS state
		  ON state.instance_id=claim.instance_id
		 AND state.scope='claim-fairness'
		 AND state.key=claim.gaggle||'|'||claim.provider||'|'||claim.external_id
		WHERE claim.instance_id='test-instance' AND claim.external_id='fairness'`,
	).Scan(&claimXID, &stateXID); err != nil {
		t.Fatal(err)
	}
	if claimXID != stateXID {
		t.Fatalf("claim xid %s != fairness state xid %s", claimXID, stateXID)
	}
}

func TestSchedulerStateRejectsBackwardCursor(t *testing.T) {
	store, _ := testStore(t, "controller")
	id := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	future := time.Now().UTC().Add(time.Hour)
	if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{id: future}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{id: future.Add(-time.Minute)}); err == nil {
		t.Fatal("scheduler cursor moved backward")
	}
}

func TestLockedExpiredRetryCannotCreateTwoHolders(t *testing.T) {
	store, pool := testStore(t, "controller")
	key := claimKey("locked-expired")
	if ok, _, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	locker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(t.Context()) }()
	if _, err := locker.Exec(t.Context(), `
		UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND gaggle=$2 AND provider=$3 AND external_id=$4`,
		store.instanceID, key.Gaggle, key.Provider, key.ExternalID,
	); err != nil {
		t.Fatal(err)
	}
	type result struct {
		ok     bool
		holder string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		ok, holder, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour)
		done <- result{ok: ok, holder: holder, err: err}
	}()
	waitForClaimsLock(t, pool, store.instanceID)
	if err := locker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	retry := <-done
	if retry.err != nil || !retry.ok || retry.holder != "run-a" {
		t.Fatalf("retry = %v %q %v", retry.ok, retry.holder, retry.err)
	}
	ok, holder, err := store.ClaimScoped(key, "run-b", "implementation", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ok || holder != "run-a" {
		t.Fatalf("second holder acquired after skipped retry: ok=%v holder=%q", ok, holder)
	}
}

func TestStoreRejectsLeaseFromAnotherInstance(t *testing.T) {
	store, _ := testStore(t, "controller")
	if _, err := New(store.pool, "other-instance", store.manager, store.handle); err == nil {
		t.Fatal("instance B accepted instance A lease handle")
	}
}

func TestVerificationMatchesFileBackendRejections(t *testing.T) {
	store, pool := testStore(t, "controller")
	key := claimKey("verification")
	if ok, _, err := store.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	entry := store.Snapshot()[0]
	observedAt := databaseNow(t, pool)
	valid := localscheduler.ClaimVerification{
		State: "verified", ObservedAt: observedAt, ProviderRunID: "run-a",
	}
	if ok, err := store.RecordClaimVerification(entry, valid); err != nil || !ok {
		t.Fatalf("valid observation = %v %v", ok, err)
	}
	for _, observation := range []localscheduler.ClaimVerification{
		{State: "verified", ObservedAt: valid.ObservedAt.Add(-time.Nanosecond), ProviderRunID: "run-a"},
		{State: "verified", ObservedAt: observedAt.Add(time.Hour), ProviderRunID: "run-a"},
		{State: "verified", ObservedAt: observedAt, ProviderRunID: "other"},
	} {
		ok, err := store.RecordClaimVerification(entry, observation)
		if observation.ObservedAt.After(observedAt) || observation.ProviderRunID == "other" {
			if !errors.Is(err, localscheduler.ErrInvalidClaimVerification) || ok {
				t.Fatalf("invalid observation accepted: %+v = %v %v", observation, ok, err)
			}
		} else if err != nil || ok {
			t.Fatalf("older observation = %v %v, want unchanged", ok, err)
		}
	}
	if err := store.manager.Release(t.Context(), store.handle); err != nil {
		t.Fatal(err)
	}
	successor := successorStore(t, pool, "successor")
	if ok, err := successor.RecordClaimVerification(entry, localscheduler.ClaimVerification{
		State: "missing", ObservedAt: databaseNow(t, pool),
	}); err != nil || ok {
		t.Fatalf("predecessor observation = %v %v, want unchanged", ok, err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.claim SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND external_id=$2`,
		successor.instanceID, key.ExternalID,
	); err != nil {
		t.Fatal(err)
	}
	if ok, err := successor.RecordClaimVerification(entry, localscheduler.ClaimVerification{
		State: "missing", ObservedAt: databaseNow(t, pool),
	}); err != nil || ok {
		t.Fatalf("expired observation = %v %v, want unchanged", ok, err)
	}
}

func TestSuccessorAdoptsPredecessorClaimsBeforeRenewAndRelease(t *testing.T) {
	old, pool := testStore(t, "old")
	key := claimKey("adopt")
	if ok, _, err := old.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.lease_heartbeat
		SET expires_at=clock_timestamp()-interval '1 second'
		WHERE instance_id=$1 AND name=$2`,
		old.handle.InstanceID, old.handle.Name,
	); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, pool, "new")
	adopted, err := current.AdoptClaims(old.handle.Epoch)
	if err != nil || adopted != 1 {
		t.Fatalf("adopted = %d, %v", adopted, err)
	}
	if ok, _, err := current.ClaimScoped(key, "run-a", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("renew adopted claim = %v %v", ok, err)
	}
	if err := current.ReleaseScoped(key, "run-a", claimrelease.Completed.Reason()); err != nil {
		t.Fatalf("release adopted claim: %v", err)
	}
}

func TestExpiredRowsDoNotBlockFreshAdmission(t *testing.T) {
	store, pool := testStore(t, "controller")
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.claim
			(instance_id,gaggle,provider,external_id,run_id,workflow,owner_token,owner_epoch,
			 claimed_at,renewed_at,expires_at)
		SELECT $1,'g','github','expired-'||n,'run-'||n,'implementation','token-'||n,$2,
		       clock_timestamp()-interval '2 hours',clock_timestamp()-interval '2 hours',
		       clock_timestamp()-interval '1 hour'
		FROM generate_series(1,$3) AS n`,
		store.instanceID, store.handle.Epoch, MaxClaims,
	); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.ClaimScoped(
		claimKey("fresh-after-expiry"), "fresh-run", "implementation", time.Hour,
	); err != nil || !ok {
		t.Fatalf("fresh admission = %v %v", ok, err)
	}
}

func TestTriggerCursorSurvivesOmission(t *testing.T) {
	store, _ := testStore(t, "controller")
	id := localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "w"}
	at := time.Now().UTC()
	if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{id: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadTriggerEvaluations("")
	if err != nil || !got[id].Equal(at) {
		t.Fatalf("omission lost cursor: %v %v", got, err)
	}
	if err := store.WriteTriggerEvaluations("", map[localscheduler.WorkflowIdentity]time.Time{
		id: at.Add(-time.Hour),
	}); err == nil {
		t.Fatal("reintroduced cursor moved backward")
	}
}

func waitForClaimsLock(t *testing.T, pool *pgxpool.Pool, instanceID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var acquired bool
		err = tx.QueryRow(t.Context(), `
			SELECT pg_try_advisory_xact_lock(hashtextextended('claims:' || $1, 0))`,
			instanceID,
		).Scan(&acquired)
		_ = tx.Rollback(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !acquired {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("claim retry did not reach the locked row")
}

func databaseNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func claimKey(id string) localscheduler.ClaimKey {
	return localscheduler.ClaimKey{Gaggle: "g", Provider: "github", ExternalID: id}
}

func successorStore(t *testing.T, pool *pgxpool.Pool, holder string) *Store {
	t.Helper()
	manager, err := cloudlease.New(cloudlease.Config{
		Pool: pool, InstanceID: "test-instance", Name: "control", Holder: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(pool, "test-instance", manager, handle)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testStore(t *testing.T, holder string) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	databaseName := "claims_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = cloudlease.MaxFencedConnections
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, pool)
	if err := (migrate.Runner{
		Direct: pool, Catalog: catalog, BackupRef: "claims-test",
		InstanceID: "migration", Holder: "migration",
	}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := successorStore(t, pool, holder)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	return store, pool
}
