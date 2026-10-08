package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/internal/sqliteuri"
)

func TestChildCustodyUpgradePreservesAdmittedReceiptAndReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admitted.db")
	db, err := sql.Open("sqlite", sqliteuri.File(path)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	// Version 5 is the preceding admission slice, before artifact custody.
	if err := sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:5]); err != nil {
		t.Fatal(err)
	}
	before := &Store{db: db}
	request := reservedChildRequest("existing-parent")
	child := acceptChildTest(t, before, request, childTestTime)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	after := openTestStore(t, path)
	retained, duplicate, err := after.AcceptChild(t.Context(), request, childTestTime)
	if err != nil || !duplicate || retained.ChildID != child.ChildID || retained.AcceptanceID != child.AcceptanceID {
		t.Fatalf("upgrade lost accepted identity: %+v duplicate=%v err=%v", retained, duplicate, err)
	}
	proposal, err := after.ChildProposal(t.Context(), child.Identity)
	if err != nil || string(proposal.Source) != string(request.Proposal.Source) {
		t.Fatalf("upgrade changed retained source: %v", err)
	}
	if got := childReservedBytes(t, after, child.ChildID); got != childCompletionAllowance {
		t.Fatalf("upgrade changed completion capacity: %d", got)
	}
	if _, err := after.ChildSnapshot(t.Context(), child.Identity); !errors.Is(err, ErrChildSnapshotPending) {
		t.Fatalf("upgrade invented a fork: %v", err)
	}
	if _, err := after.ChildResult(t.Context(), child.Identity); !errors.Is(err, ErrChildResultPending) {
		t.Fatalf("upgrade invented a result: %v", err)
	}
	if err := after.KeepChildSnapshot(t.Context(), child, childSnapshotTestValue("fork", "bundle")); err != nil {
		t.Fatal("upgraded admission cannot retain its fork", err)
	}
	if err := after.KeepChildResult(t.Context(), child, childResultValue("result", "result-bundle")); err != nil {
		t.Fatal("upgraded admission cannot retain its result", err)
	}
	// An admission-only binary must refuse the newer store without changing it.
	if err := sqliteschema.Migrate(t.Context(), after.db, "triggerqueue", migrations[:5]); err == nil || !strings.Contains(err.Error(), "newer than this build supports") {
		t.Fatalf("older schema writer did not refuse: %v", err)
	}
	if _, err := after.ChildResult(t.Context(), child.Identity); err != nil {
		t.Fatal("downgrade refusal damaged retained result", err)
	}
}
