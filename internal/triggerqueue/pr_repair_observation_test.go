package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/sqliteschema"
	"github.com/goobers/goobers/providers"
)

func TestPRRepairObservationPreservesReceiptAcrossRaceReopenAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q := openTestStore(t, path)
	input := repairInputFixture(t, q)
	r := repairComplete(t, q, repairAccept(t, q, input), "unknown")
	original := *r.Receipt
	if _, err := q.CloseSession(t.Context(), sessionCommand("close"), input.Origin.SessionID, "stop", childTestTime); err != nil {
		t.Fatal(err)
	}
	checker := sessioning.Actor{Issuer: "reviewer", Subject: "other-human"}
	observedAt := childTestTime.Add(100 * 24 * time.Hour)
	other := openTestStore(t, path)
	var wg sync.WaitGroup
	for _, store := range []*Store{q, other} {
		wg.Go(func() {
			got, err := store.ObservePRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, checker, providers.PRRepairObservation{Matches: true, CommitID: strings.Repeat("c", 40)}, observedAt)
			if err != nil || got.State != "observed-applied" {
				t.Error(got, err)
			}
		})
	}
	wg.Wait()
	// A late identical completion cannot regress the derived state or acknowledgement.
	got, err := q.CompletePRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, original, observedAt.Add(time.Second))
	if err != nil || got.State != "observed-applied" || !reflect.DeepEqual(*got.Receipt, original) || got.Receipt.ProviderAcknowledged {
		t.Fatal(got, err)
	}
	got, err = other.PRRepairCommandForReview(t.Context(), input.Scope.Gaggle, r.ID)
	if err != nil || len(got.Observations) != 1 || got.Observations[0].Checker != checker || got.ProvenCommit() != strings.Repeat("c", 40) {
		t.Fatal(got, err)
	}
	tx, err := q.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyPRRepairWriterCustody(t.Context(), tx, input.Selection); err != nil {
		t.Fatal("proof did not release target", err)
	}
	_ = tx.Rollback()
	if n, err := q.PrunePRRepairCommands(t.Context(), observedAt.Add(29*24*time.Hour), 100); err != nil || n != 0 {
		t.Fatal("new proof lost review window", n, err)
	}
	if n, err := q.PrunePRRepairCommands(t.Context(), observedAt.Add(31*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = q.PRRepairCommandForReview(t.Context(), input.Scope.Gaggle, r.ID); !errors.Is(err, ErrWorkbenchCommandExpired) {
		t.Fatal(err)
	}
	if n, err := q.PrunePRRepairCommands(t.Context(), observedAt.Add(62*24*time.Hour), 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestPRRepairObservationBoundsUnprovenHistoryAndRefusesAttempting(t *testing.T) {
	q := openTestStore(t, filepath.Join(t.TempDir(), "q.db"))
	input := repairInputFixture(t, q)
	r := repairAccept(t, q, input)
	r, _, err := q.ClaimPRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	checker := sessioning.Actor{Issuer: "issuer", Subject: "checker"}
	if _, err = q.ObservePRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, checker, providers.PRRepairObservation{}, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("unjoined effect observed", err)
	}
	r, err = q.CompletePRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, sessioning.PRRepairReceipt{OperationDigest: input.OperationDigest, Outcome: "unknown", MutationAttempted: true}, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		r, err = q.ObservePRRepairCommand(t.Context(), input.Scope, r.ID, r.RequestDigest, checker, providers.PRRepairObservation{}, childTestTime.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.State != "unknown" || len(r.Observations) != 16 || r.OmittedObservations != 9 || r.ProvenCommit() != "" {
		t.Fatal(r)
	}
	tx, _ := q.db.BeginTx(t.Context(), nil)
	err = verifyPRRepairWriterCustody(t.Context(), tx, input.Selection)
	_ = tx.Rollback()
	if !errors.Is(err, ErrConflict) {
		t.Fatal("negative check released custody", err)
	}
	if _, err = q.db.Exec(`UPDATE pr_repair_commands SET observed_ns=? WHERE id=?`, childTestTime.UnixNano(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = q.PRRepairCommand(t.Context(), input.Scope, r.ID); err == nil {
		t.Fatal("tampered settlement accepted")
	}
}

func TestPRRepairObservationMigrationIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, prRepairObservationSchema)
	if index < 1 {
		t.Fatal(index)
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES('kept','kept','human','{}','accepted',1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	q := openTestStore(t, path)
	var got string
	if err = q.db.QueryRow(`SELECT actor FROM triggers WHERE id='kept'`).Scan(&got); err != nil || got != "human" {
		t.Fatal(got, err)
	}
	input := repairInputFixture(t, q)
	r := repairComplete(t, q, repairAccept(t, q, input), "unknown")
	if len(r.Observations) != 0 || r.State != "unknown" {
		t.Fatal(r)
	}
}
