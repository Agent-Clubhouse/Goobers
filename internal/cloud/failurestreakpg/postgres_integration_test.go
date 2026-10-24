//go:build cloudintegration

package failurestreakpg_test

import (
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/providers"
)

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

// The memory of which runs charged a streak must be appended on the UPDATE path
// too, not only when the row is first inserted.
func TestEveryChargingRunIsRememberedIncludingOnTheUpdatePath(t *testing.T) {
	pool := pgtest.Pool(t, "fspg")
	controller := pgtest.ControllerPool(t, pool)
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	store := failurestreakpg.Store{Pool: controller, Fence: manager}
	charged := func(run string) bool {
		ok, err := store.ChargedBy(t.Context(), pgtest.InstanceID, failurestreakpg.Work, repo, "42", run)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if charged("r1") {
		t.Fatal("a run charged an item with no streak")
	}
	for i, run := range []string{"r1", "r2", "r3"} { // r1 inserts; r2 and r3 take the update path
		if err := store.Write(t.Context(), handle, failurestreakpg.Work, repo, "42", i+1, run, "ci"); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{"r1", "r2", "r3"} {
		if !charged(run) {
			t.Errorf("%s charged the streak but is not remembered", run)
		}
	}
	if charged("r4") {
		t.Error("a run that never charged is reported as charged")
	}
	// The other kind is a separate streak.
	if ok, _ := store.ChargedBy(t.Context(), pgtest.InstanceID, failurestreakpg.Infra, repo, "42", "r1"); ok {
		t.Error("the work streak's runs leaked into the infrastructure streak")
	}
}

// Only the most recent MaxRecentRuns runs are remembered.
func TestTheRunMemoryIsCapped(t *testing.T) {
	pool := pgtest.Pool(t, "fspg")
	controller := pgtest.ControllerPool(t, pool)
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	store := failurestreakpg.Store{Pool: controller, Fence: manager}
	total := failurestreakpg.MaxRecentRuns + 4
	for i := 0; i < total; i++ {
		if err := store.Write(t.Context(), handle, failurestreakpg.Work, repo, "42", i+1, fmt.Sprintf("r%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	var length int
	if err := controller.QueryRow(t.Context(), `SELECT cardinality(recent_runs) FROM control.failure_streak`).Scan(&length); err != nil || length != failurestreakpg.MaxRecentRuns {
		t.Fatalf("remembered %d runs (%v), want the cap of %d", length, err, failurestreakpg.MaxRecentRuns)
	}
	for i := 0; i < total; i++ {
		ok, err := store.ChargedBy(t.Context(), pgtest.InstanceID, failurestreakpg.Work, repo, "42", fmt.Sprintf("r%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if want := i >= total-failurestreakpg.MaxRecentRuns; ok != want {
			t.Errorf("r%d remembered = %v, want %v", i, ok, want)
		}
	}
	// A re-write of a remembered run moves it to the newest slot without growing.
	if err := store.Write(t.Context(), handle, failurestreakpg.Work, repo, "42", total+1, fmt.Sprintf("r%d", total-1), ""); err != nil {
		t.Fatal(err)
	}
	if err := controller.QueryRow(t.Context(), `SELECT cardinality(recent_runs) FROM control.failure_streak`).Scan(&length); err != nil || length != failurestreakpg.MaxRecentRuns {
		t.Fatalf("remembered %d runs after a rewrite (%v)", length, err)
	}
}
