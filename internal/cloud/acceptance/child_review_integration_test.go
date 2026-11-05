//go:build cloudintegration

package acceptance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func finishReviewChild(t *testing.T, store *Store, child triggerqueue.ChildRecord) {
	t.Helper()
	result := childDigest([]byte("result"))
	if err := store.SetChildState(t.Context(), child.Identity,
		triggerqueue.ChildStateUpdate{
			Expected: triggerqueue.ChildQueued,
			State:    triggerqueue.ChildFailed, ResultRef: result,
		}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeChild(
		t.Context(), child.Identity, result, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestChildControllerPruningPreservesCustody(t *testing.T) {
	store, pool := childTestStore(t)
	request := childRequest("parent", "stage", "call")
	child, _, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	finishReviewChild(t, store, child)
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.acceptance
		SET accepted_at=clock_timestamp()-interval '8 days',
		    finished_at=clock_timestamp()-interval '8 days'
		WHERE instance_id=$1 AND id=$2`,
		store.instanceID, child.AcceptanceID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(
		t.Context(), "ordinary", "actor", []byte("ordinary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetChild(t.Context(), child.Identity); err != nil {
		t.Fatalf("ordinary pruning erased child custody: %v", err)
	}
}

func TestChildTombstoneResolvesExactReplay(t *testing.T) {
	store, pool := childTestStore(t)
	request := childRequest("parent", "stage", "call")
	child, _, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	finishReviewChild(t, store, child)
	if err := store.MarkChildParentSettled(
		t.Context(), child.Identity.ChildParent, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.child_parent
		SET settled_at=clock_timestamp()-interval '31 days'
		WHERE instance_id=$1`, store.instanceID); err != nil {
		t.Fatal(err)
	}
	result, err := store.PruneChildren(t.Context(), time.Now(), 100)
	if err != nil || result.Tombstoned != 1 {
		t.Fatalf("prune = %+v %v", result, err)
	}
	// Ordinary controller retention must also preserve the acceptance identity
	// for the full child tombstone lifetime, not just while lineage is live.
	if _, err := pool.Exec(t.Context(), `UPDATE control.acceptance
 SET accepted_at=clock_timestamp()-interval '8 days', finished_at=clock_timestamp()-interval '8 days'
 WHERE instance_id=$1 AND id=$2`, store.instanceID, child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, duplicate, err := store.AcceptChild(t.Context(), request, time.Now())
	if err != nil || !duplicate || replay.TombstonedAt.IsZero() {
		t.Fatalf("tombstone replay = %+v duplicate=%v err=%v",
			replay, duplicate, err)
	}
}

func TestChildRevocationSerializesBeforeAdmission(t *testing.T) {
	store, pool := childTestStore(t)
	grant := reviewAuthority("parent", "stage")
	if err := store.BindChildAuthority(t.Context(), grant, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(t.Context(), `
		SELECT owner_epoch FROM control.child_parent
		WHERE instance_id=$1 AND gaggle='g' AND parent_run_id='parent'
		FOR UPDATE`, store.instanceID); err != nil {
		t.Fatal(err)
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- store.RevokeChildAuthority(t.Context(), grant) }()
	select {
	case err := <-revokeDone:
		t.Fatalf("revocation bypassed parent lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	request := childRequest("parent", "stage", "call")
	request.Authority = &grant
	admitDone := make(chan error, 1)
	go func() {
		_, _, err := store.AcceptChild(t.Context(), request, time.Now())
		admitDone <- err
	}()
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-admitDone; !errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
		t.Fatalf("admission after revocation = %v", err)
	}
}

func TestChildAdmissionSharesOrdinaryCapacity(t *testing.T) {
	store, pool := childTestStore(t)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO control.acceptance (
			instance_id,id,idem_key,actor,fingerprint,payload,state,run_id,accepted_at
		)
		SELECT $1,'fill-'||n,'fill-'||n,'actor',
		       decode('00','hex'),decode('78','hex'),'accepted','fill-'||n,
		       clock_timestamp()
		FROM generate_series(1,$2) AS n`,
		store.instanceID, triggerqueue.MaxRecords,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AcceptChild(
		t.Context(), childRequest("parent", "stage", "call"), time.Now(),
	); !errors.Is(err, triggerqueue.ErrFull) {
		t.Fatalf("child bypassed ordinary capacity: %v", err)
	}
}

func TestChildAuthorityPruneSkipsLivePrefix(t *testing.T) {
	store, pool := childTestStore(t)
	for _, name := range []string{"a-live", "z-expired"} {
		grant := reviewAuthority(name, "stage")
		if name == "z-expired" {
			grant.ExpiresAt = time.Now().Add(-time.Hour)
		}
		if err := store.BindChildAuthority(
			t.Context(), grant, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.PruneChildren(t.Context(), time.Now(), 1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM control.child_authority
		WHERE instance_id=$1 AND parent_run_id='z-expired'`,
		store.instanceID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("expired authority hidden behind live prefix")
	}
}

func TestChildPendingPublicationPinsRetention(t *testing.T) {
	store, pool := childTestStore(t)
	child, _, err := store.AcceptChild(
		t.Context(), childRequest("parent", "stage", "call"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	publication, err := store.PrepareChildPublication(
		t.Context(), child.Identity, "branch", []byte(`{"intent":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginChildPublicationEffect(
		t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	finishReviewChild(t, store, child)
	if err := store.MarkChildParentSettled(
		t.Context(), child.Identity.ChildParent, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE control.child_parent
		SET settled_at=clock_timestamp()-interval '31 days'
		WHERE instance_id=$1`, store.instanceID); err != nil {
		t.Fatal(err)
	}
	result, err := store.PruneChildren(t.Context(), time.Now(), 100)
	if err != nil || result.Tombstoned != 0 {
		t.Fatalf("pending publication pruned = %+v %v", result, err)
	}
}

func reviewAuthority(parent, occurrence string) triggerqueue.ChildAuthority {
	return triggerqueue.ChildAuthority{
		ChildParent: triggerqueue.ChildParent{
			Gaggle: "g", ParentRunID: parent,
		},
		StageOccurrence: occurrence,
		GrantID:         "grant", AttemptID: "attempt",
		ConfigDigest: childDigest([]byte("config")),
		PolicyDigest: childDigest([]byte("policy")),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}
