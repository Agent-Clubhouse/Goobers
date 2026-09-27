package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedIncompleteReservation writes the exact debris a crashed publish leaves
// behind on a production instance: a reservation directory holding only
// `.publish.lock` and `snapshot.bundle.lock`, and no record.json (#5177).
//
// age backdates the directory and its files rather than waiting: the
// reconciler reads modification times, never elapsed wall-clock time.
func seedIncompleteReservation(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	for _, file := range []string{".publish.lock", BundleFileName + ".lock"} {
		path := filepath.Join(directory, file)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// Last: writing the files above bumped the directory's own mtime.
	if err := os.Chtimes(directory, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return directory
}

func reservationTestName(index int) string {
	return fmt.Sprintf("%064x", index)
}

func TestReconcileReclaimsStaleDebrisAndRetainsInFlightReservations(t *testing.T) {
	root := t.TempDir()
	record := storageTestRecord()
	published := seedInventoryRecord(t, root, record)
	stale := seedIncompleteReservation(t, root, reservationTestName(1), 4*IncompleteReservationGrace)
	fresh := seedIncompleteReservation(t, root, reservationTestName(2), 0)

	results, err := ReconcileIncompleteReservations(context.Background(), root, 8, IncompleteReservationGrace, false)
	if err != nil || len(results) != 2 {
		t.Fatalf("dry run: %+v %v", results, err)
	}
	for _, result := range results {
		switch result.Path {
		case stale:
			if !result.Stale || !result.DryRun || result.Deleted {
				t.Fatalf("dry run did not report the stale candidate: %+v", result)
			}
		case fresh:
			if result.Stale || result.Deleted {
				t.Fatalf("in-flight reservation reported as reclaimable: %+v", result)
			}
		default:
			t.Fatalf("unexpected reconcile result: %+v", result)
		}
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("dry run deleted a candidate: %v", err)
	}

	results, err = ReconcileIncompleteReservations(context.Background(), root, 8, IncompleteReservationGrace, true)
	if err != nil || len(results) != 2 {
		t.Fatalf("reconcile: %+v %v", results, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale debris survived reconciliation: %v", err)
	}
	// An in-flight publish must keep its reservation — and keep counting
	// toward capacity, which is what makes reserving over it impossible.
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("reconciliation raced an in-flight publish: %v", err)
	}
	if _, err := ReadRecord(filepath.Join(published, RecordFileName)); err != nil {
		t.Fatalf("reconciliation touched a published record: %v", err)
	}
}

func TestReconcileLeavesUnrecognizedInventoryContentAlone(t *testing.T) {
	root := t.TempDir()
	foreign := seedIncompleteReservation(t, root, reservationTestName(3), 4*IncompleteReservationGrace)
	if err := os.WriteFile(filepath.Join(foreign, "operator-evidence"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(root, "manually-quarantined")
	if err := os.Mkdir(named, 0o700); err != nil {
		t.Fatal(err)
	}
	results, err := ReconcileIncompleteReservations(context.Background(), root, 8, IncompleteReservationGrace, true)
	if err != nil || len(results) != 0 {
		t.Fatalf("unrecognized content was considered reclaimable: %+v %v", results, err)
	}
	for _, path := range []string{foreign, named, filepath.Join(foreign, "operator-evidence")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("reconciliation removed unrecognized content %s: %v", path, err)
		}
	}
}

// The wedged state this exists to clear holds MORE entries than the cap
// ("130 of 128 slots used", where the read itself refuses). Reconciliation
// must still run there, or the only automatic way out is unavailable exactly
// when it is needed.
func TestReconcileRunsOnAnInventoryOverItsCap(t *testing.T) {
	root := t.TempDir()
	const limit = 2
	for i := 0; i < limit+3; i++ {
		seedIncompleteReservation(t, root, reservationTestName(i), 4*IncompleteReservationGrace)
	}
	if _, err := ReadInventory(context.Background(), root, limit); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("fixture is not over the cap: %v", err)
	}
	results, err := ReconcileIncompleteReservations(context.Background(), root, limit, IncompleteReservationGrace, true)
	if err != nil || len(results) != limit+3 {
		t.Fatalf("over-cap reconcile: %d results, %v", len(results), err)
	}
	entries, err := ReadInventory(context.Background(), root, limit)
	if err != nil || len(entries) != 0 {
		t.Fatalf("inventory still refuses its own read after reconciliation: %+v %v", entries, err)
	}
}

// Heal-on-upgrade (#5354): an instance whose inventory is already full of
// lock-only debris must admit its next publish with no operator action. The
// reservation is taken through the real publication entry point; beforePublish
// stops it just after the slot is granted, so the assertion is about capacity
// rather than about git bundle production.
func TestPublicationHealsAnInventoryFullOfIncompleteReservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{name: "stale-debris-is-reclaimed", age: 4 * IncompleteReservationGrace},
		{name: "in-flight-reservations-still-hold-capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repository := t.TempDir(), t.TempDir()
			const limit = 2
			var debris []string
			for i := 0; i < limit; i++ {
				debris = append(debris, seedIncompleteReservation(t, root, reservationTestName(i), tc.age))
			}
			admitted := errors.New("reservation admitted")
			_, _, err := publishToInventory(context.Background(), repository, root, []string{repository},
				storageTestRecord(), limit, 1<<20, func() error { return admitted }, nil)
			if tc.age == 0 {
				if !errors.Is(err, ErrInventoryFull) {
					t.Fatalf("in-flight reservations stopped counting toward capacity: %v", err)
				}
				return
			}
			if !errors.Is(err, admitted) {
				t.Fatalf("full-of-debris inventory refused the next publication: %v", err)
			}
			for _, path := range debris {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("debris survived the capacity reclaim: %v", err)
				}
			}
		})
	}
}

func TestReconcileRejectsInvalidBoundsAndMissingInventory(t *testing.T) {
	root := t.TempDir()
	if _, err := ReconcileIncompleteReservations(context.Background(), root, 0, IncompleteReservationGrace, true); err == nil {
		t.Fatal("invalid limit accepted")
	}
	if _, err := ReconcileIncompleteReservations(context.Background(), root, 8, 0, true); err == nil {
		t.Fatal("invalid stale window accepted")
	}
	absent := filepath.Join(root, "absent")
	if results, err := ReconcileIncompleteReservations(context.Background(), absent, 8, IncompleteReservationGrace, true); err != nil || len(results) != 0 {
		t.Fatalf("missing inventory: %+v %v", results, err)
	}
	if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciliation created the inventory: %v", err)
	}
}
