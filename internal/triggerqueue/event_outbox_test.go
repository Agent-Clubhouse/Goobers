package triggerqueue

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

func TestEventPublicationOutboxRecoveryConflictRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	store := openTestStore(t, path)
	now := time.Now().UTC()
	req := eventRequest("outbox-1", false)
	req.Producer.RunID = "producer-run"
	req.Producer.RootID = "producer-run"
	req.Producer.Stage = "publish"
	intent := EventPublication{ID: "outbox-1", ConfigGeneration: "generation-1", Occurrence: "occurrence-1", Acceptance: req}
	first, duplicate, err := store.BeginEventPublication(t.Context(), intent, now)
	if err != nil || duplicate {
		t.Fatal(first, duplicate, err)
	}
	// Producer custody predates receipt intake, including a process interruption.
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	intent.Acceptance.Plan = eventing.Plan{Revision: "changed", Routes: []eventing.Route{eventRoute("other", "", 0, 0, 0)}}
	retry, duplicate, err := store.BeginEventPublication(t.Context(), intent, now.Add(time.Minute))
	if err != nil || !duplicate || retry.Acceptance.Plan.Revision != first.Acceptance.Plan.Revision {
		t.Fatal(retry, duplicate, err)
	}
	changed := intent
	changed.Acceptance.Envelope = []byte(strings.Replace(string(req.Envelope), "42", "43", 1))
	if _, _, err = store.BeginEventPublication(t.Context(), changed, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("payload conflict=%v", err)
	}
	receipt, _, err := store.AcceptEvent(t.Context(), retry.Acceptance, now)
	if err != nil {
		t.Fatal(err)
	}
	// Losing the reply before the outbox acknowledgement cannot let retention
	// delete the original receipt and recreate an already accepted publication.
	if _, err = store.PruneEvents(t.Context(), now.Add(90*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	retained, err := store.Event(t.Context(), req.Producer.Gaggle, req.Producer.Binding, receipt.ID)
	if err != nil || len(retained.Envelope) == 0 {
		t.Fatal(retained, err)
	}
	duplicateReceipt, duplicate, err := store.AcceptEvent(t.Context(), retry.Acceptance, now)
	if err != nil || !duplicate || duplicateReceipt.ID != receipt.ID {
		t.Fatal(duplicateReceipt, duplicate, err)
	}
	if err = store.CompleteEventPublication(t.Context(), retry, receipt); err != nil {
		t.Fatal(err)
	}
	page, err := store.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 1 || page[0].ReceiptID != receipt.ID {
		t.Fatal(page, err)
	}
	forged := receipt
	forged.Producer.Gaggle = "other"
	if err = store.CompleteEventPublication(t.Context(), retry, forged); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
