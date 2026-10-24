//go:build cloudintegration

package claims

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestStoreClaimReadErrorsAsControllerRole(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TEST_POSTGRES_URL")
	fixture := newPlaneFixture(t) // Asserts current_user=goobers_controller.
	replica := fixture.leader(t, "read-errors")
	store, err := New(fixture.controller, pgtest.InstanceID, replica.manager, replica.handle.Load())
	if err != nil {
		t.Fatal(err)
	}
	reads := storeReads(store)
	// Empty is a valid successful result, distinct from every failure below.
	for name, read := range reads {
		got, err := read(t.Context())
		if err != nil || len(got) != 0 {
			t.Fatalf("empty %s=%+v, %v", name, got, err)
		}
	}
	for _, key := range []string{"a-valid", "z-invalid"} {
		if ok, _, err := store.ClaimScoped(claimKey(key), "run-a", "implementation", time.Hour); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	for name, read := range reads {
		got, err := read(t.Context())
		if err != nil || len(got) != 2 {
			t.Fatalf("healthy %s=%+v, %v", name, got, err)
		}
	}
	// Query errors are exercised under runtime grants, not an owner session.
	for name, read := range reads {
		table := "control.claim"
		if name == "history" {
			table = "control.claim_run_history"
		}
		if _, err := fixture.owner.Exec(t.Context(), "REVOKE SELECT (verification) ON "+table+" FROM goobers_controller"); err != nil {
			t.Fatal(err)
		}
		got, readErr := read(t.Context())
		_, restoreErr := fixture.owner.Exec(t.Context(), "GRANT SELECT (verification) ON "+table+" TO goobers_controller")
		if restoreErr != nil {
			t.Fatal(restoreErr)
		}
		var denied *pgconn.PgError
		if got != nil || !errors.Is(readErr, localscheduler.ErrClaimRead) || !errors.As(readErr, &denied) || denied.Code != "42501" {
			t.Fatalf("%s permission failure=%+v, %v", name, got, readErr)
		}
	}
	// Valid JSON with the wrong shape fails during row decoding, after a valid
	// active row. No read may publish that partial prefix as a usable snapshot.
	for _, table := range []string{"control.claim", "control.claim_run_history"} {
		if _, err := fixture.owner.Exec(t.Context(), "UPDATE "+table+" SET verification='\"not-a-claim\"'::jsonb WHERE instance_id=$1 AND external_id='z-invalid'", pgtest.InstanceID); err != nil {
			t.Fatal(err)
		}
	}
	for name, read := range reads {
		got, err := read(t.Context())
		var malformed *json.UnmarshalTypeError
		if got != nil || !errors.Is(err, localscheduler.ErrClaimRead) || !errors.As(err, &malformed) {
			t.Fatalf("%s malformed row=%+v, %v", name, got, err)
		}
	}
	// Even filtering to a run with no claims cannot hide the snapshot failure.
	got, err := store.ForRunAllErr(t.Context(), "absent")
	if got != nil || !errors.Is(err, localscheduler.ErrClaimRead) {
		t.Fatalf("absent run hid corruption: %+v, %v", got, err)
	}
	listing, err := replica.plane.ListNamespace(t.Context(), "g", "github")
	if listing.Entries != nil || listing.History != nil || !errors.Is(err, localscheduler.ErrClaimRead) {
		t.Fatalf("namespace hid corruption: %+v, %v", listing, err)
	}
	// Repair only the active row: the namespace must still refuse a failed
	// history read rather than returning active claims with an empty history.
	if _, err := fixture.owner.Exec(t.Context(), "UPDATE control.claim SET verification=NULL WHERE instance_id=$1", pgtest.InstanceID); err != nil {
		t.Fatal(err)
	}
	listing, err = replica.plane.ListNamespace(context.Background(), "g", "github")
	if listing.Entries != nil || listing.History != nil || !errors.Is(err, localscheduler.ErrClaimRead) {
		t.Fatalf("namespace hid history failure: %+v, %v", listing, err)
	}
}
