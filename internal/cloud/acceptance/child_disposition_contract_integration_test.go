//go:build cloudintegration

package acceptance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// One child-disposition contract every generated-child custody store must
// satisfy, run against the local SQLite store and this cloud store (as the
// controller runtime role). A retried parent attempt must revise its child's
// disposition the same way on both, or attempt replacement is not equivalent
// across backends (#8085 b2).
func TestChildDispositionContract(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		runDispositionContract(t, func(t *testing.T) dispositionBackend {
			path := filepath.Join(t.TempDir(), "queue.db")
			store, err := triggerqueue.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return dispositionBackend{store: store, tamper: func(t *testing.T, childID string, edit bool, revision int) {
				t.Helper()
				// Behind the store's back, as an operator with file access could.
				db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				if !edit {
					result, err := db.ExecContext(t.Context(), `DELETE FROM child_disposition_history WHERE child_id=? AND revision=?`, childID, revision)
					mustAffectOne(t, result, err)
					return
				}
				var receipt []byte
				if err := db.QueryRowContext(t.Context(), `SELECT receipt FROM child_disposition_history WHERE child_id=? AND revision=?`, childID, revision).Scan(&receipt); err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(receipt, &record); err != nil {
					t.Fatal(err)
				}
				record["action"] = "replace"
				if receipt, err = json.Marshal(record); err != nil {
					t.Fatal(err)
				}
				result, err := db.ExecContext(t.Context(), `UPDATE child_disposition_history SET receipt=? WHERE child_id=? AND revision=?`, receipt, childID, revision)
				mustAffectOne(t, result, err)
			}}
		})
	})
	t.Run("postgres", func(t *testing.T) {
		runDispositionContract(t, func(t *testing.T) dispositionBackend {
			store, owner := childTestStore(t)
			// The grants under test are the runtime role's: refuse to pass as
			// a superuser that every grant gap would hide behind.
			var role string
			if err := store.pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&role); err != nil || role != "goobers_controller" {
				t.Fatalf("contract runs as %q (%v), want goobers_controller", role, err)
			}
			return dispositionBackend{store: store, tamper: func(t *testing.T, childID string, edit bool, revision int) {
				t.Helper()
				// As the database owner, which the controller role is not.
				query := `DELETE FROM control.child_disposition_history WHERE instance_id=$1 AND child_id=$2 AND revision=$3`
				if edit {
					query = `UPDATE control.child_disposition_history SET action='replace' WHERE instance_id=$1 AND child_id=$2 AND revision=$3`
				}
				tag, err := owner.Exec(t.Context(), query, store.instanceID, childID, revision)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("tamper affected %d rows (%v), want 1", tag.RowsAffected(), err)
				}
			}}
		})
	})
}

func mustAffectOne(t *testing.T, result sql.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("tamper affected %d rows (%v), want 1", n, err)
	}
}

// dispositionBackend is one store under the contract, plus a way to edit
// (edit) or delete (!edit) one retained history row behind its back.
type dispositionBackend struct {
	store  dispositionStore
	tamper func(t *testing.T, childID string, edit bool, revision int)
}

// dispositionStore is the custody surface the contract drives.
type dispositionStore interface {
	BindChildAuthority(ctx context.Context, grant triggerqueue.ChildAuthority, expectedGrant string, now time.Time) error
	AcceptChild(ctx context.Context, request triggerqueue.ChildAcceptance, now time.Time) (triggerqueue.ChildRecord, bool, error)
	AcknowledgeChild(ctx context.Context, identity triggerqueue.ChildIdentity, expectedResultRef string, now time.Time) error
	KeepChildResult(ctx context.Context, child triggerqueue.ChildRecord, result triggerqueue.ChildResult) error
	SetChildState(ctx context.Context, identity triggerqueue.ChildIdentity, update triggerqueue.ChildStateUpdate, now time.Time) error
	RequestChildDisposition(ctx context.Context, request triggerqueue.ChildDispositionRequest, now time.Time) (triggerqueue.ChildDisposition, error)
	ChildDisposition(ctx context.Context, identity triggerqueue.ChildIdentity) (triggerqueue.ChildDisposition, error)
	KeepChildDispositionPlan(ctx context.Context, expected triggerqueue.ChildDisposition, plan []byte) error
	CompleteChildDisposition(ctx context.Context, expected triggerqueue.ChildDisposition, now time.Time) error
	ChildDispositionHistory(ctx context.Context, identity triggerqueue.ChildIdentity) ([]triggerqueue.ChildDispositionRevision, error)
}

// runDispositionContract runs the contract. open returns a fresh, empty
// store for each subtest.
func runDispositionContract(t *testing.T, open func(*testing.T) dispositionBackend) {
	t.Run("revisions are CAS-ordered and a published plan is fenced", func(t *testing.T) { dispositionRevisions(t, open(t).store) })
	t.Run("a stale non-empty digest cannot revise", func(t *testing.T) { dispositionStaleDigest(t, open(t).store) })
	t.Run("a replacement attempt rebinds the same choice", func(t *testing.T) { dispositionReplacement(t, open(t).store) })
	t.Run("an acknowledged child takes no new choice", func(t *testing.T) { dispositionAcknowledged(t, open(t).store) })
	t.Run("an unapplied choice blocks acknowledgement", func(t *testing.T) { dispositionAcknowledgedUnapplied(t, open(t).store) })
	t.Run("history is bounded", func(t *testing.T) { dispositionHistoryBound(t, open(t).store) })
	t.Run("an edited history row breaks the chain", func(t *testing.T) { dispositionChainTamper(t, open(t), true, 1) })
	t.Run("a deleted first history row breaks the chain", func(t *testing.T) { dispositionChainTamper(t, open(t), false, 1) })
	// The remaining rows still link; only the current request's predecessor
	// digest shows the gap.
	t.Run("a deleted last history row breaks the chain", func(t *testing.T) { dispositionChainTamper(t, open(t), false, 2) })
}

// contractClock advances a second per call, so a store that takes the caller's time
// and one that uses its own both see strictly ordered revisions.
type contractClock struct{ now time.Time }

func (c *contractClock) next() time.Time {
	c.now = c.now.Add(time.Second)
	return c.now
}

func contractDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func contractAuthority(parent triggerqueue.ChildParent, occurrence, attempt, grant string, expires time.Time) triggerqueue.ChildAuthority {
	return triggerqueue.ChildAuthority{ChildParent: parent, StageOccurrence: occurrence, GrantID: grant, AttemptID: attempt,
		ConfigDigest: "sha256:" + strings.Repeat("a", 64), PolicyDigest: "sha256:" + strings.Repeat("b", 64), ExpiresAt: expires}
}

// dispositionContractFixture accepts one child under attempt A's authority and takes it to a
// terminal result, ready for a disposition.
func dispositionContractFixture(t *testing.T, s dispositionStore, c *contractClock) (triggerqueue.ChildRecord, triggerqueue.ChildDispositionRequest) {
	t.Helper()
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: "0123456789abcdef0123456789abcdef"},
		StageOccurrence: "stage", InvocationKey: "key"}
	grant := contractAuthority(identity.ChildParent, identity.StageOccurrence, "attempt-a", "grant-a", c.now.Add(time.Hour))
	if err := s.BindChildAuthority(t.Context(), grant, "", c.next()); err != nil {
		t.Fatalf("bind attempt A: %v", err)
	}
	acceptance := triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage",
		Payload: []byte(`{"request":{"workflow":"child","gaggle":"own"}}`), MaxChildren: 2, Authority: &grant}
	child, duplicate, err := s.AcceptChild(t.Context(), acceptance, c.next())
	if err != nil || duplicate {
		t.Fatalf("accept = %+v, %v, %v", child, duplicate, err)
	}
	receipt := []byte("terminal")
	result := triggerqueue.ChildResult{Receipt: receipt, ReceiptDigest: contractDigest(receipt)}
	if err := s.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatalf("keep result: %v", err)
	}
	if err := s.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued,
		State: triggerqueue.ChildFailed, ResultRef: result.ReceiptDigest}, c.next()); err != nil {
		t.Fatalf("terminal state: %v", err)
	}
	return child, triggerqueue.ChildDispositionRequest{Identity: child.Identity, Action: "merge", ResultRef: result.ReceiptDigest, Authority: grant}
}

func dispositionRevisions(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	first, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.RequestChildDisposition(t.Context(), request, c.next()); err != nil || again.RequestDigest() != first.RequestDigest() {
		t.Fatalf("lost response of the first request = %+v, %v", again, err)
	}
	changed := request
	changed.Action = "discard"
	if _, err := s.RequestChildDisposition(t.Context(), changed, c.next()); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("unconditional replacement = %v, want ErrConflict", err)
	}
	changed.ExpectedRequestDigest = first.RequestDigest()
	second, err := s.RequestChildDisposition(t.Context(), changed, c.next())
	if err != nil || second.Action != "discard" || second.PreviousDigest != first.RequestDigest() {
		t.Fatalf("revision = %+v, %v", second, err)
	}
	if retry, err := s.RequestChildDisposition(t.Context(), changed, c.next()); err != nil || retry.RequestDigest() != second.RequestDigest() {
		t.Fatalf("lost response changed the revision: %+v, %v", retry, err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, c.next()); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("stale original replaced the latest = %v, want ErrConflict", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), first, []byte("stale plan")); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("a superseded request kept a plan = %v, want ErrConflict", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), second, []byte("exact plan")); err != nil {
		t.Fatal(err)
	}
	changed.Action, changed.ExpectedRequestDigest = "replace", second.RequestDigest()
	if _, err := s.RequestChildDisposition(t.Context(), changed, c.next()); !errors.Is(err, triggerqueue.ErrChildDispositionReconcile) {
		t.Fatalf("a published plan was replaced = %v, want ErrChildDispositionReconcile", err)
	}
	history, err := s.ChildDispositionHistory(t.Context(), request.Identity)
	if err != nil || len(history) != 1 || history[0].RequestDigest != first.RequestDigest() || history[0].Action != "merge" {
		t.Fatalf("history = %+v, %v; want the first request archived", history, err)
	}
}

func dispositionReplacement(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	first, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), first, []byte("exact plan")); err != nil {
		t.Fatal(err)
	}
	planned, err := s.ChildDisposition(t.Context(), request.Identity)
	if err != nil {
		t.Fatal(err)
	}
	// The parent's stage attempt is replaced; the new attempt re-requests the
	// same choice naming the current request digest.
	next := contractAuthority(request.Identity.ChildParent, request.Identity.StageOccurrence, "attempt-b", "grant-b", c.now.Add(time.Hour))
	if err := s.BindChildAuthority(t.Context(), next, "grant-a", c.next()); err != nil {
		t.Fatalf("bind attempt B: %v", err)
	}
	retried := request
	retried.Authority, retried.ExpectedRequestDigest = next, planned.RequestDigest()
	rebound, err := s.RequestChildDisposition(t.Context(), retried, c.next())
	if err != nil || rebound.AttemptID != "attempt-b" || rebound.PlanDigest != planned.PlanDigest || string(rebound.Plan) != "exact plan" ||
		rebound.PreviousDigest != planned.RequestDigest() {
		t.Fatalf("rebind = %+v, %v; want attempt B owning the exact plan", rebound, err)
	}
	if again, err := s.RequestChildDisposition(t.Context(), retried, c.next()); err != nil || again.RequestDigest() != rebound.RequestDigest() {
		t.Fatalf("lost response of the rebind = %+v, %v", again, err)
	}
	if err := s.CompleteChildDisposition(t.Context(), planned, c.next()); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("the replaced attempt acknowledged = %v, want ErrConflict", err)
	}
	if err := s.CompleteChildDisposition(t.Context(), rebound, c.next()); err != nil {
		t.Fatal(err)
	}
	// An applied choice is final, but an exact repeat of it is still answered.
	if again, err := s.RequestChildDisposition(t.Context(), retried, c.next()); err != nil || again.RequestDigest() != rebound.RequestDigest() || again.AppliedAt.IsZero() {
		t.Fatalf("repeat after application = %+v, %v", again, err)
	}
	discard := retried
	discard.Action, discard.ExpectedRequestDigest = "discard", rebound.RequestDigest()
	if _, err := s.RequestChildDisposition(t.Context(), discard, c.next()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("revising an applied choice = %v, want ErrTransition", err)
	}
	history, err := s.ChildDispositionHistory(t.Context(), request.Identity)
	if err != nil || len(history) != 1 || history[0].RequestDigest != planned.RequestDigest() || history[0].PlanDigest != planned.PlanDigest ||
		history[0].AttemptID != "attempt-a" {
		t.Fatalf("history = %+v, %v; want attempt A's planned request archived", history, err)
	}
}

func dispositionHistoryBound(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	current, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	actions := []string{"discard", "merge"}
	for i := 0; i < triggerqueue.MaxChildDispositionRevisions; i++ {
		revised := request
		revised.Action, revised.ExpectedRequestDigest = actions[i%2], current.RequestDigest()
		if current, err = s.RequestChildDisposition(t.Context(), revised, c.next()); err != nil {
			t.Fatalf("revision %d: %v", i+1, err)
		}
	}
	full := request
	full.Action, full.ExpectedRequestDigest = "replace", current.RequestDigest()
	if _, err := s.RequestChildDisposition(t.Context(), full, c.next()); !errors.Is(err, triggerqueue.ErrChildDispositionHistoryFull) {
		t.Fatalf("revision past the bound = %v, want ErrChildDispositionHistoryFull", err)
	}
	history, err := s.ChildDispositionHistory(t.Context(), request.Identity)
	if err != nil || len(history) != triggerqueue.MaxChildDispositionRevisions {
		t.Fatalf("history holds %d (%v), want %d", len(history), err, triggerqueue.MaxChildDispositionRevisions)
	}
}

// Only the current request digest revises: the digest of a request that was
// itself superseded is a conflict, however well-formed.
func dispositionStaleDigest(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	first, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	revised := request
	revised.Action, revised.ExpectedRequestDigest = "discard", first.RequestDigest()
	second, err := s.RequestChildDisposition(t.Context(), revised, c.next())
	if err != nil {
		t.Fatal(err)
	}
	stale := request
	stale.Action, stale.ExpectedRequestDigest = "replace", first.RequestDigest()
	if _, err := s.RequestChildDisposition(t.Context(), stale, c.next()); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("revision from the superseded digest = %v, want ErrConflict", err)
	}
	current, err := s.ChildDisposition(t.Context(), request.Identity)
	if err != nil || current.RequestDigest() != second.RequestDigest() {
		t.Fatalf("current = %+v, %v; the stale revision must not land", current, err)
	}
}

// Once a terminal child is acknowledged, a choice it never had cannot start,
// and an unapplied one cannot be revised.
func dispositionAcknowledged(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	if err := s.AcknowledgeChild(t.Context(), request.Identity, request.ResultRef, c.next()); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, c.next()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("first choice for an acknowledged child = %v, want ErrTransition", err)
	}
}

func dispositionAcknowledgedUnapplied(t *testing.T, s dispositionStore) {
	c := &contractClock{now: time.Now().UTC()}
	_, request := dispositionContractFixture(t, s, c)
	first, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	// A requested choice holds the occurrence until it is applied, so neither
	// store acknowledges past it; the revision path's acknowledged guard is
	// the backstop for custody that got there anyway.
	if err := s.AcknowledgeChild(t.Context(), request.Identity, request.ResultRef, c.next()); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatalf("acknowledging past an unapplied choice = %v, want ErrTransition", err)
	}
	revised := request
	revised.Action, revised.ExpectedRequestDigest = "discard", first.RequestDigest()
	if again, err := s.RequestChildDisposition(t.Context(), revised, c.next()); err != nil || again.PreviousDigest != first.RequestDigest() {
		t.Fatalf("the unacknowledged choice stays revisable: %+v, %v", again, err)
	}
}

// ChildDispositionHistory verifies the digest chain it returns: a row edited
// or deleted outside the store is reported, never served.
func dispositionChainTamper(t *testing.T, b dispositionBackend, edit bool, revision int) {
	c := &contractClock{now: time.Now().UTC()}
	s := b.store
	child, request := dispositionContractFixture(t, s, c)
	current, err := s.RequestChildDisposition(t.Context(), request, c.next())
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"discard", "merge"} {
		revised := request
		revised.Action, revised.ExpectedRequestDigest = action, current.RequestDigest()
		if current, err = s.RequestChildDisposition(t.Context(), revised, c.next()); err != nil {
			t.Fatal(err)
		}
	}
	if history, err := s.ChildDispositionHistory(t.Context(), request.Identity); err != nil || len(history) != 2 {
		t.Fatalf("history = %d records, %v; want 2", len(history), err)
	}
	b.tamper(t, child.ChildID, edit, revision)
	if history, err := s.ChildDispositionHistory(t.Context(), request.Identity); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatalf("tampered history = %+v, %v; want ErrConflict", history, err)
	}
}
