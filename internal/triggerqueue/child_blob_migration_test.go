package triggerqueue

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

func TestChildBlobUpgradePreservesPendingDisposition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disposition.db")
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	// Version 9 already owns result disposition and its bounded revision history.
	if err := sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:9]); err != nil {
		t.Fatal(err)
	}
	before := &Store{db: db}
	child, request := dispositionFixture(t, before, "returned-parent", childTestTime)
	choice, err := before.RequestChildDisposition(t.Context(), request, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.KeepChildDispositionPlan(t.Context(), choice, []byte("retained application plan")); err != nil {
		t.Fatal(err)
	}
	remaining := childReservedBytes(t, before, child.ChildID)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after := openTestStore(t, path)
	retained, err := after.ChildDisposition(t.Context(), child.Identity)
	if err != nil || retained.RequestDigest() != choice.RequestDigest() || string(retained.Plan) != "retained application plan" {
		t.Fatalf("blob upgrade changed pending application: %+v, %v", retained, err)
	}
	if got := childReservedBytes(t, after, child.ChildID); got != remaining {
		t.Fatalf("upgrade charged unused blob capacity: %d, want %d", got, remaining)
	}
	if childTableCount(t, after, "child_blobs") != 0 || childTableCount(t, after, "child_blob_reads") != 0 {
		t.Fatal("upgrade invented blob custody")
	}
	if err := sqliteschema.Migrate(t.Context(), after.db, "triggerqueue", migrations[:9]); err == nil {
		t.Fatal("older disposition binary accepted the blob schema")
	}
	// An in-flight application must remain completable after upgrade/refused downgrade.
	if err := after.CompleteChildDisposition(t.Context(), retained, childTestTime); err != nil {
		t.Fatal(err)
	}
}
