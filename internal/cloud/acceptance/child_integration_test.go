//go:build cloudintegration

package acceptance

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blobstore"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildRunIDFromAcceptance(t *testing.T) {
	store, pool := childTestStore(t)
	request := childRequest("parent", "stage/visit-1", "call-1")
	before := databaseTime(t, pool)
	child, duplicate, err := store.AcceptChild(
		t.Context(), request, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	after := databaseTime(t, pool)
	if err != nil || duplicate {
		t.Fatalf("accept child = %+v %v %v", child, duplicate, err)
	}
	var acceptanceRunID string
	if err := pool.QueryRow(t.Context(), `
		SELECT run_id FROM control.acceptance
		WHERE instance_id=$1 AND id=$2`,
		store.instanceID, child.AcceptanceID,
	).Scan(&acceptanceRunID); err != nil {
		t.Fatal(err)
	}
	if child.RunID != acceptanceRunID ||
		child.AcceptanceID != "trigger-"+child.RunID ||
		child.ChildID != "child-"+child.RunID {
		t.Fatalf("child identity = %+v, acceptance run = %s", child, acceptanceRunID)
	}
	if child.AcceptedAt.Before(before) || child.AcceptedAt.After(after) {
		t.Fatalf("accepted_at = %s, database interval [%s,%s]",
			child.AcceptedAt, before, after)
	}
}

func TestChildRetryResolvesBeforeFenceAndCapacity(t *testing.T) {
	store, pool := childTestStore(t)
	request := childRequest("parent", "stage", "call")
	child, _, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FenceChildParent(
		t.Context(), request.Identity.ChildParent, "operator", time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.child_occurrence
		SET accepted_count=max_children
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3 AND occurrence=$4`,
		store.instanceID, request.Identity.Gaggle, request.Identity.ParentRunID,
		request.Identity.StageOccurrence,
	); err != nil {
		t.Fatal(err)
	}
	replay, duplicate, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil || !duplicate || replay.ChildID != child.ChildID ||
		!replay.CancellationRequested {
		t.Fatalf("retry = %+v %v %v", replay, duplicate, err)
	}
	changed := request
	changed.Payload = []byte("different")
	if _, _, err := store.AcceptChild(t.Context(), changed, time.Now()); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("changed retry = %v, want ErrConflict", err)
	}
}

func TestChildOccurrenceConcurrentClaimOneWinner(t *testing.T) {
	store, _ := childTestStore(t)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for index := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := childRequest("parent", "stage", fmt.Sprintf("call-%d", index))
			<-start
			_, _, err := store.AcceptChild(t.Context(), request, time.Now())
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var accepted, occupied int
	for err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, triggerqueue.ErrChildSlotOccupied):
			occupied++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 1 || occupied != 1 {
		t.Fatalf("accepted=%d occupied=%d", accepted, occupied)
	}
}

func TestChildSuccessorAdoptionFencesPredecessor(t *testing.T) {
	old, _ := childTestStore(t)
	request := childRequest("parent", "stage", "call")
	child, _, err := old.AcceptChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	predecessorEpoch := old.handle.Epoch
	if err := old.manager.Release(t.Context(), old.handle); err != nil {
		t.Fatal(err)
	}
	current := successorStore(t, old.pool)
	current.BindChildBlobs(old.childBlobs)
	if err := current.BeginDispatch(t.Context(), child.AcceptanceID); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("dispatch before adoption = %v", err)
	}
	adopted, err := current.AdoptChildren(t.Context(), predecessorEpoch)
	if err != nil || adopted < 3 {
		t.Fatalf("adopted=%d err=%v", adopted, err)
	}
	if err := current.BeginDispatch(t.Context(), child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := current.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{
		Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.AcceptChild(
		t.Context(), childRequest("parent", "other", "call"), time.Now(),
	); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale predecessor acceptance = %v", err)
	}
}

func TestChildDigestCustodySurvivesStoreReopen(t *testing.T) {
	store, _ := childTestStore(t)
	request := childRequest("parent", "stage", "call")
	proposal := []byte("workflow: child")
	request.Proposal = &triggerqueue.ChildProposal{
		Digest: childDigest(proposal), Source: proposal,
	}
	child, _, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := triggerqueue.ChildSnapshot{
		Receipt: []byte("snapshot receipt"), Bundle: []byte("snapshot bundle"),
	}
	snapshot.ReceiptDigest = childDigest(snapshot.Receipt)
	snapshot.BundleDigest = childDigest(snapshot.Bundle)
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(store.pool, store.instanceID)
	if err != nil {
		t.Fatal(err)
	}
	reopened.BindFence(store.manager, store.handle)
	reopened.BindChildBlobs(store.childBlobs)
	gotProposal, err := reopened.ChildProposal(t.Context(), child.Identity)
	if err != nil || string(gotProposal.Source) != string(proposal) {
		t.Fatalf("proposal = %+v %v", gotProposal, err)
	}
	gotSnapshot, err := reopened.ChildSnapshot(t.Context(), child.Identity)
	if err != nil || string(gotSnapshot.Bundle) != string(snapshot.Bundle) {
		t.Fatalf("snapshot = %+v %v", gotSnapshot, err)
	}
}

func TestChildPublicationCustodyIsIdempotent(t *testing.T) {
	store, _ := childTestStore(t)
	child, _, err := store.AcceptChild(
		t.Context(), childRequest("parent", "stage", "call"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	intent := []byte(`{"branch":"child"}`)
	publication, err := store.PrepareChildPublication(
		t.Context(), child.Identity, "branch", intent)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.PrepareChildPublication(
		t.Context(), child.Identity, "branch", intent)
	if err != nil || replay.Digest != publication.Digest {
		t.Fatalf("publication replay = %+v %v", replay, err)
	}
	if err := store.BeginChildPublicationEffect(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ChildPublication(t.Context(), child.Identity, "branch")
	if err != nil || pending.State != "effect_pending" {
		t.Fatalf("pending publication = %+v %v", pending, err)
	}
	receipt := []byte(`{"url":"https://example.invalid/branch"}`)
	if err := store.ConfirmChildPublication(t.Context(), pending, receipt); err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.ChildPublication(t.Context(), child.Identity, "branch")
	if err != nil || confirmed.State != "confirmed" ||
		string(confirmed.Receipt) != string(receipt) {
		t.Fatalf("confirmed publication = %+v %v", confirmed, err)
	}
}

func TestChildStoreRejectsAnotherInstanceHandle(t *testing.T) {
	store, _ := childTestStore(t)
	other, err := New(store.pool, "other-instance")
	if err != nil {
		t.Fatal(err)
	}
	other.BindFence(store.manager, store.handle)
	other.BindChildBlobs(store.childBlobs)
	if _, _, err := other.AcceptChild(
		t.Context(), childRequest("parent", "stage", "call"), time.Now(),
	); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("cross-instance handle = %v, want ErrTransition", err)
	}
}

func TestChildRetentionUsesDatabaseClockAndReleasesBlobPointers(t *testing.T) {
	store, pool := childTestStore(t)
	child, _, err := store.AcceptChild(
		t.Context(), childRequest("parent", "stage", "call"), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := triggerqueue.ChildSnapshot{
		Receipt: []byte("receipt"), Bundle: []byte("bundle"),
	}
	snapshot.ReceiptDigest = childDigest(snapshot.Receipt)
	snapshot.BundleDigest = childDigest(snapshot.Bundle)
	if err := store.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal(err)
	}
	resultRef := "sha256:" + fmt.Sprintf("%064x", 1)
	if err := store.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{
		Expected: triggerqueue.ChildQueued,
		State:    triggerqueue.ChildFailed, ResultRef: resultRef,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeChild(t.Context(), child.Identity, resultRef, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.child_parent
		SET settled_at=clock_timestamp()-interval '31 days'
		WHERE instance_id=$1 AND gaggle=$2 AND parent_run_id=$3`,
		store.instanceID, child.Identity.Gaggle, child.Identity.ParentRunID,
	); err != nil {
		t.Fatal(err)
	}
	result, err := store.PruneChildren(t.Context(), time.Now().Add(365*24*time.Hour), 100)
	if err != nil || result.Tombstoned != 1 {
		t.Fatalf("tombstone = %+v %v", result, err)
	}
	var blobPointers int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.child_blob
		WHERE instance_id=$1 AND child_id=$2`,
		store.instanceID, child.ChildID,
	).Scan(&blobPointers); err != nil {
		t.Fatal(err)
	}
	if blobPointers != 0 {
		t.Fatalf("tombstone retained %d blob pointers", blobPointers)
	}
	if _, err := store.ChildSnapshot(t.Context(), child.Identity); !errors.Is(err, triggerqueue.ErrChildSnapshotUnavailable) {
		t.Fatalf("tombstoned snapshot = %v", err)
	}
}

func childTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	store, pool := testStore(t, 1)
	if err := store.manager.Release(t.Context(), store.handle); err != nil {
		t.Fatal(err)
	}
	// Only fixture backdating and lock orchestration use the owner connection.
	// Every shipped child operation and its lease execute with inventory grants.
	store = successorStore(t, childRolePool(t, pool, "goobers_controller"))
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.BindChildBlobs(blobs)
	return store, pool
}

func childRequest(parent, occurrence, invocation string) triggerqueue.ChildAcceptance {
	return triggerqueue.ChildAcceptance{
		Identity: triggerqueue.ChildIdentity{
			ChildParent: triggerqueue.ChildParent{
				Gaggle: "g", ParentRunID: parent,
			},
			StageOccurrence: occurrence, InvocationKey: invocation,
		},
		Actor: "parent-stage", Payload: []byte(`{"request":"child"}`), MaxChildren: 2,
	}
}

func databaseTime(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now.UTC()
}
