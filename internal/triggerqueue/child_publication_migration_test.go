package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

func TestChildPublicationUpgradePreservesCustody(t *testing.T) {
	for _, version := range []int{8, 9, 10, 11} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.db")
			db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			db.SetMaxOpenConns(1)
			if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:version]); err != nil {
				t.Fatal(err)
			}
			before := &Store{db: db}
			child := seedLegacyChild(t, before, reservedChildRequest("publication-parent"), childTestTime)
			result := childResultValue("retained-result", "retained-bundle")
			if err = before.KeepChildResult(t.Context(), child, result); err != nil {
				t.Fatal(err)
			}
			credit := childReservedBytes(t, before, child.ChildID)
			if version == 8 {
				// Schema 9 separately adds disposition history credit.
				if _, err = db.Exec(`UPDATE child_lineages SET reserved_bytes=reserved_bytes-? WHERE child_id=?`, childDispositionHistoryAllowance, child.ChildID); err != nil {
					t.Fatal(err)
				}
			}
			if err = before.Close(); err != nil {
				t.Fatal(err)
			}
			after := openTestStore(t, path)
			retained, err := after.ChildResult(t.Context(), child.Identity)
			if err != nil || retained.ReceiptDigest != result.ReceiptDigest || string(retained.Bundle) != "retained-bundle" {
				t.Fatal("upgrade changed result", err)
			}
			if got := childReservedBytes(t, after, child.ChildID); got != credit {
				t.Fatalf("upgrade changed reservation: %d want %d", got, credit)
			}
			if _, err = after.ChildPublication(t.Context(), child.Identity, "branch"); !errors.Is(err, ErrChildPublicationPending) {
				t.Fatal("upgrade invented publication", err)
			}
			intent, err := after.PrepareChildExecutionPublication(t.Context(), child.Identity, child.RunID, "branch", []byte(`{"head":"owned"}`))
			if err != nil || intent.ExecutionRunID != child.RunID {
				t.Fatal("publication emitter not pinned", intent, err)
			}
			if err = sqliteschema.Migrate(t.Context(), after.db, "triggerqueue", migrations[:version]); err == nil {
				t.Fatal("old writer accepted publication schema")
			}
			got, err := after.ChildPublication(t.Context(), child.Identity, "branch")
			if err != nil || got.ExecutionRunID != child.RunID || got.Digest != intent.Digest {
				t.Fatal("downgrade refusal changed intent", got, err)
			}
		})
	}
}
