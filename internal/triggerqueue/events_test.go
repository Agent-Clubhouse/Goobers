package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

func eventRequest(id string, matched bool) EventAcceptance {
	req := EventAcceptance{Producer: EventProducer{Gaggle: "web", Binding: "ingress", Actor: "binding:ingress"}, Envelope: []byte(fmt.Sprintf(`{"specversion":"1.0","id":%q,"source":"/factory","type":"pr.changed","data":{"pr":42}}`, id)), Plan: eventing.Plan{Revision: "routing-1"}}
	if matched {
		req.Plan.Routes = []eventing.Route{{Consumer: "repair", Revision: "subscription-1", Workflow: "repair", WorkflowDigest: "workflow-1", GooberDigest: "goober-1", ConfigGeneration: "generation-1", Debounce: &eventing.Debounce{Key: "pr:42", Window: time.Second, MaxWait: 10 * time.Second, MaxEvents: 100, InputMode: "all"}}}
	}
	return req
}

func TestEventCustodyRetryAcrossReopenAndScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	req := eventRequest("1", true)
	first, duplicate, err := store.AcceptEvent(t.Context(), req, now)
	if err != nil || duplicate || first.State != EventRoutingPending {
		t.Fatalf("accept: %+v %v %v", first, duplicate, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	// A changed active configuration cannot silently reroute a retry.
	req.Plan = eventing.Plan{Revision: "routing-2"}
	req.Envelope = []byte(`{ "type":"pr.changed", "data":{"pr":42}, "source":"/factory", "id":"1", "specversion":"1.0" }`)
	retry, duplicate, err := store.AcceptEvent(t.Context(), req, now.Add(time.Minute))
	if err != nil || !duplicate || retry.ID != first.ID || string(retry.Plan) != string(first.Plan) || !retry.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("retry: %+v %v %v", retry, duplicate, err)
	}
	for _, mutate := range []func(*EventAcceptance){
		func(r *EventAcceptance) { r.Producer.Actor = "other" },
		func(r *EventAcceptance) { r.Envelope = []byte(strings.Replace(string(r.Envelope), "42", "43", 1)) },
	} {
		changed := req
		mutate(&changed)
		if _, _, err := store.AcceptEvent(t.Context(), changed, now); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflict: %v", err)
		}
	}
	if _, err := store.Event(t.Context(), "other", "ingress", first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-gaggle: %v", err)
	}
	if _, err := store.Event(t.Context(), "web", "other", first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-binding: %v", err)
	}
	req.Producer.Gaggle = "other"
	other, duplicate, err := store.AcceptEvent(t.Context(), req, now)
	if err != nil || duplicate || other.ID == first.ID || other.State != EventUnmatched {
		t.Fatalf("independent scope: %+v %v %v", other, duplicate, err)
	}
}

func TestEventConcurrentDuplicateKeepsOneReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	left, right := openTestStore(t, path), openTestStore(t, path)
	var wg sync.WaitGroup
	results := make(chan EventReceipt, 12)
	for i := range 12 {
		wg.Go(func() {
			store := left
			if i%2 != 0 {
				store = right
			}
			receipt, _, err := store.AcceptEvent(t.Context(), eventRequest("concurrent", false), time.Now())
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			results <- receipt
		})
	}
	wg.Wait()
	close(results)
	var id string
	for receipt := range results {
		if id != "" && receipt.ID != id {
			t.Fatal("duplicate receipt created")
		}
		id = receipt.ID
	}
	var count int
	if err := left.db.QueryRow(`SELECT COUNT(*) FROM event_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d, %v", count, err)
	}
}

func TestEventRetentionSteadyStateAndRoutingDeadline(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	now := time.Now().UTC()
	for cycle := range 5 {
		req := eventRequest(fmt.Sprintf("unmatched-%d", cycle), false)
		receipt, _, err := store.AcceptEvent(t.Context(), req, now)
		if err != nil || receipt.State != EventUnmatched {
			t.Fatalf("unmatched: %+v %v", receipt, err)
		}
		pending, _, err := store.AcceptEvent(t.Context(), eventRequest(fmt.Sprintf("matched-%d", cycle), true), now)
		if err != nil {
			t.Fatal(err)
		}
		pruned, err := store.PruneEvents(t.Context(), now.Add(EventRoutingDeadline), 1)
		if err != nil || pruned.Expired != 1 || pruned.Tombstoned != 0 {
			t.Fatalf("routing deadline: %+v %v", pruned, err)
		}
		failed, err := store.Event(t.Context(), "web", "ingress", pending.ID)
		if err != nil || failed.State != EventRoutingFailed || failed.Reason != "routing deadline exceeded" {
			t.Fatalf("deadline custody: %+v %v", failed, err)
		}
		now = now.Add(EventRoutingDeadline + EventRetention)
		pruned, err = store.PruneEvents(t.Context(), now, 100)
		if err != nil || pruned.Tombstoned != 2 {
			t.Fatalf("tombstone: %+v %v", pruned, err)
		}
		tomb, duplicate, err := store.AcceptEvent(t.Context(), req, now)
		if err != nil || !duplicate || tomb.ID != receipt.ID || tomb.TombstonedAt.IsZero() || len(tomb.Envelope) != 0 || len(tomb.Plan) != 0 {
			t.Fatalf("tombstone retry: %+v %v %v", tomb, duplicate, err)
		}
		now = now.Add(EventTombstoneRetention)
		pruned, err = store.PruneEvents(t.Context(), now, 100)
		if err != nil || pruned.Deleted != 2 {
			t.Fatalf("delete: %+v %v", pruned, err)
		}
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM event_receipts`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unbounded rows=%d %v", count, err)
		}
	}
}

func TestEventIntakePreservesOtherCustodyReservations(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
	now := time.Now().UTC()
	req := eventRequest("reserved", true)
	accepted, _, err := store.AcceptEvent(t.Context(), req, now)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate occupied future completion credit without allocating huge files.
	if _, err := store.db.Exec(`UPDATE event_receipts SET reserved_bytes=? WHERE id=?`, childStoreByteCeiling, accepted.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AcceptEvent(t.Context(), eventRequest("new", false), now); !errors.Is(err, ErrFull) {
		t.Fatalf("event stole custody reserve: %v", err)
	}
	if _, _, err := store.Accept(t.Context(), "manual", "operator", []byte(`{}`), now); !errors.Is(err, ErrFull) {
		t.Fatalf("manual stole custody reserve: %v", err)
	}
	if got, duplicate, err := store.AcceptEvent(t.Context(), req, now); err != nil || !duplicate || got.ID != accepted.ID {
		t.Fatalf("full-store replay: %+v %v %v", got, duplicate, err)
	}
	pruned, err := store.PruneEvents(t.Context(), now.Add(EventRoutingDeadline), 100)
	if err != nil || pruned.Expired != 1 {
		t.Fatalf("finalization: %+v %v", pruned, err)
	}
	if _, _, err := store.AcceptEvent(t.Context(), eventRequest("new", false), now.Add(EventRoutingDeadline)); err != nil {
		t.Fatalf("released reservation: %v", err)
	}
}

func TestEventCorruptCustodyCannotBeReplacedByRetry(t *testing.T) {
	for _, column := range []string{"envelope", "plan"} {
		t.Run(column, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "events.db"))
			req := eventRequest("corrupt", true)
			accepted, _, err := store.AcceptEvent(t.Context(), req, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			// column is a fixed test-case identifier, not user input.
			if _, err := store.db.Exec("UPDATE event_receipts SET "+column+"=? WHERE id=?", []byte(`{}`), accepted.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Event(t.Context(), "web", "ingress", accepted.ID); err == nil {
				t.Fatal("corruption read as accepted evidence")
			}
			if _, _, err := store.AcceptEvent(t.Context(), req, time.Now()); err == nil {
				t.Fatal("retry recaptured corrupt custody")
			}
		})
	}
}
