package triggerqueue

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestEventRoutingMigratesReceiptsWithoutInventingGenerationPins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var beforeGroups []string
	for _, migration := range migrations {
		if migration == eventGroupSchema {
			break
		}
		beforeGroups = append(beforeGroups, migration)
	}
	if len(beforeGroups) == len(migrations) {
		t.Fatal("event group migration not registered")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", beforeGroups); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-unpinned", "pinned"} {
		req := eventRequest(id, true)
		envelope, authority, plan, err := validateEventAcceptance(req, childTestTime)
		if err != nil {
			t.Fatal(err)
		}
		if id == "legacy-unpinned" {
			plan = []byte(strings.Replace(string(plan), `,"configGeneration":"generation-1"`, "", 1))
		}
		_, err = db.Exec(`INSERT INTO event_receipts(id,gaggle,producer,source,event_id,authority,digest,envelope,plan,plan_digest,state,accepted_ns,reserved_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, req.Producer.Gaggle, req.Producer.Binding, envelope.Source, envelope.ID, authority, envelope.Digest, envelope.JSON, plan, "sha256:"+childDigest(plan), EventRoutingPending, childTestTime.UnixNano(), 4096)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	first := routeEventTest(t, s, childTestTime)
	if first.ReceiptID != "legacy-unpinned" || first.State != EventRoutingFailed || len(first.Groups) != 0 {
		t.Fatalf("legacy source rematched current configuration: %+v", first)
	}
	second := routeEventTest(t, s, childTestTime)
	if second.ReceiptID != "pinned" || second.State != EventRouted || len(second.Groups) != 1 {
		t.Fatalf("pinned source lost: %+v", second)
	}
	newReceipt := acceptRoutedEvent(t, s, "after-migration", childTestTime, eventRoute("repair", "", 0, 0, 0))
	if newReceipt.Sequence != 3 {
		t.Fatalf("receipt sequence reset: %d", newReceipt.Sequence)
	}
	var reserved int
	if err = s.db.QueryRow(`SELECT SUM(reserved_starts) FROM event_receipts`).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("routing credits were not transferred once: %d %v", reserved, err)
	}
}
