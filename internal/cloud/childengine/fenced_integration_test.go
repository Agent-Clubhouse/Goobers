//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// unusedCredential lets the object-store client be built without contacting
// any service; these tests never reach a generation archive.
type unusedCredential struct{}

func (unusedCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("unused")
}

func fencedFor(t *testing.T, f *family, owner func() *cloudlease.Handle) Fenced {
	t.Helper()
	return fencedOn(t, f, f.pool, owner)
}

func fencedOn(t *testing.T, f *family, pool *pgxpool.Pool, owner func() *cloudlease.Handle) Fenced {
	t.Helper()
	azure, err := objstore.NewAzure("http://127.0.0.1:1", "unused", unusedCredential{})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewMetadataWithOptions(pool, azure, objstore.MetadataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fenced := Fenced{Pool: pool, InstanceID: testInstance, Lease: f.manager, Owner: owner, Blobs: f.blobs, Objects: objects,
		Applied: appliedConfig{}, Wait: time.Hour, Actor: "control-plane", Now: time.Now, Adoption: &EpochAdoption{}}
	if err := fenced.Validate(); err != nil {
		t.Fatal(err)
	}
	return fenced
}

func TestFencedAdmissionsBindEveryCallToTheLiveLease(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-fenced", "call-1")
	binding := engine.ChildRunBinding{Request: in.Request}
	var owner *cloudlease.Handle
	fenced := fencedFor(t, f, func() *cloudlease.Handle { return owner })

	// A standby (or a replica mid-failover) holds no lease: retry elsewhere.
	if err := fenced.Cancel(t.Context(), binding); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("cancel without a lease = %v, want unavailable", err)
	}
	if err := f.store.CheckChildParentOpen(t.Context(), identityOf(in.Request).ChildParent); err != nil {
		t.Fatalf("a call without a lease changed custody: %v", err)
	}

	// The live owner fences the family.
	owner = f.handle
	if err := fenced.Cancel(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CheckChildParentOpen(t.Context(), identityOf(in.Request).ChildParent); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatalf("live-lease cancel did not fence the family: %v", err)
	}

	// The live owner reaches the real freezer. This fixture's retained start
	// payload is not an envelope, so the child can never freeze: a permanent
	// refusal, and nothing is bound.
	other := acceptChild(t, f, "parent-fenced-2", "call-1")
	if _, err := fenced.Admit(t.Context(), other); !errors.Is(err, engine.ErrChildCustodyRefused) {
		t.Fatalf("admit with a corrupt retained envelope = %v, want refused", err)
	}
	if _, err := f.store.ChildEngineStart(t.Context(), identityOf(other.Request)); err == nil {
		t.Fatal("a refused freeze bound a start intent")
	}

	// A lost epoch can never write: the stale handle is refused as unavailable.
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}
	if err := fenced.Cancel(t.Context(), engine.ChildRunBinding{Request: other.Request}); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("cancel under a released lease = %v, want unavailable", err)
	}
}

// controllerPool runs every statement as the controller runtime role, so the
// write-path inventory's grants are what the test exercises.
func controllerPool(t *testing.T, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// A pool that silently stayed the owner would hide every grant gap.
	var role string
	if err := pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("controller pool runs as %q (%v), want goobers_controller", role, err)
	}
	return pool
}

// A controller that takes over the lease serves its predecessor's families
// without any explicit adoption call: the first call of the new epoch adopts
// every earlier epoch's custody, fenced, as the controller role.
func TestFencedAdmissionsAdoptPredecessorCustodyAfterTakeover(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-takeover", "call-1")
	binding, err := activities(f.service(time.Hour)).AdmitEngineChild(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := f.handle.Epoch
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}
	f.takeOver(t, "controller-2")
	if f.handle.Epoch <= predecessor {
		t.Fatalf("takeover epoch %d did not advance past %d", f.handle.Epoch, predecessor)
	}
	controller := controllerPool(t, f.pool)
	fenced := fencedOn(t, f, controller, func() *cloudlease.Handle { return f.handle })
	if _, err := fenced.Load(t.Context(), binding); err != nil {
		t.Fatalf("successor could not load its predecessor's manifest: %v", err)
	}
	if err := fenced.Cancel(t.Context(), binding); err != nil {
		t.Fatalf("successor could not fence its predecessor's family: %v", err)
	}
	for _, table := range []string{"child_parent", "child_occurrence", "child_lineage", "child_blob", "child_engine_start"} {
		var stale int
		if err := f.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM control.%s WHERE instance_id=$1 AND owner_epoch<>$2`, table),
			testInstance, f.handle.Epoch).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if stale != 0 {
			t.Fatalf("control.%s kept %d rows owned by an earlier epoch", table, stale)
		}
	}
	// Adoption is idempotent: a second pass finds nothing to adopt.
	store, err := acceptance.New(controller, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(f.manager, f.handle)
	if adopted, err := store.AdoptStaleChildCustody(t.Context()); err != nil || adopted != 0 {
		t.Fatalf("second adoption = %d, %v; want 0, nil", adopted, err)
	}
}

// One Fenced, one process: it served epoch 1, lost the lease to a foreign
// controller that served epoch 2 and wrote custody, then reacquired epoch 3.
// It must adopt epoch 2's rows, not stop at its own first adoption.
func TestFencedAdmissionsReadoptOnEveryNewEpochInTheSameProcess(t *testing.T) {
	f := newFamily(t)
	ours, first := f.manager, f.handle
	first1 := acceptChild(t, f, "parent-e1", "call-1")
	b1, err := activities(f.service(time.Hour)).AdmitEngineChild(t.Context(), first1)
	if err != nil {
		t.Fatal(err)
	}
	current := first
	controller := controllerPool(t, f.pool)
	fenced := fencedOn(t, f, controller, func() *cloudlease.Handle { return current })
	fenced.Lease = ours
	if _, err := fenced.Load(t.Context(), b1); err != nil {
		t.Fatalf("epoch 1 load: %v", err)
	}

	// A foreign controller takes over and writes its own custody.
	if err := ours.Release(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	f.takeOver(t, "foreign-controller")
	foreign := acceptChild(t, f, "parent-e2", "call-1")
	b2, err := activities(f.service(time.Hour)).AdmitEngineChild(t.Context(), foreign)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Release(t.Context(), f.handle); err != nil {
		t.Fatal(err)
	}

	// This process reacquires: a new epoch on the same Fenced.
	third, err := ours.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if third.Epoch <= f.handle.Epoch {
		t.Fatalf("reacquired epoch %d did not pass the foreign epoch %d", third.Epoch, f.handle.Epoch)
	}
	current = third
	if err := fenced.Cancel(t.Context(), b2); err != nil {
		t.Fatalf("epoch 3 could not fence the foreign epoch's family: %v", err)
	}
	var stale int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND owner_epoch<>$2`,
		testInstance, third.Epoch).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("%d lineage rows left on an earlier epoch (%v)", stale, err)
	}
}
