//go:build cloudintegration

package migrate

import (
	"errors"
	"testing"
)

func TestClaimHistoryContractRefusesAlpha6AsController(t *testing.T) {
	// Seed predecessor incarnations and live claims before 0022, then finish
	// the stop-the-world upgrade with 0039. No old writer runs between them.
	pool := upgradeTo0022(t, populatedBefore0022)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Direct: pool, Catalog: catalog, BackupRef: "test", InstanceID: "migration", Holder: "test"}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	controller := controllerRolePool(t, pool)
	var retired bool
	if err := controller.QueryRow(t.Context(), `SELECT to_regclass('control.claim_history') IS NULL`).Scan(&retired); err != nil || !retired {
		t.Fatalf("predecessor table retired = %v (%v)", retired, err)
	}
	if err := CheckPoolCompatibility(t.Context(), controller, "0.6.0-alpha.6"); !errors.Is(err, ErrBinaryTooOld) || !IsIncompatible(err) {
		t.Fatalf("alpha.6 against contracted schema = %v, want ErrBinaryTooOld", err)
	}
	if err := CheckPoolCompatibility(t.Context(), controller, "0.6.0"); err != nil {
		t.Fatalf("stable release refused contracted schema: %v", err)
	}
	var total, released, live int
	if err := controller.QueryRow(t.Context(), `SELECT count(*),
		count(*) FILTER (WHERE released_at IS NOT NULL), count(*) FILTER (WHERE released_at IS NULL)
		FROM control.claim_run_history WHERE instance_id='i'`).Scan(&total, &released, &live); err != nil || total != 3 || released != 1 || live != 2 {
		t.Fatalf("retained run records total=%d released=%d live=%d (%v)", total, released, live, err)
	}
	if err := runner.Apply(t.Context()); err != nil {
		t.Fatalf("reapply contracted catalog: %v", err)
	}
}
