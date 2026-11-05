//go:build cloudintegration

package acceptance

import (
	"testing"

	"github.com/goobers/goobers/internal/cloud/startfence"
)

// UnmarkedRunPins lists a started run until the retention sweep marks it
// terminal. It carries the identity the scheduler's open-run inventory needs.
func TestPGUnmarkedRunPinsListsUntilMarkedTerminal(t *testing.T) {
	store, pool := testStore(t, 1)
	starter := startfence.New(&pgStartClient{}, "cloud", NewStartAdmission(store), testGenerationAdmission{})
	in := acceptedStartInput(t, store)
	result, err := starter.Start(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := store.UnmarkedRunPins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].RunID != result.RunID || pins[0].Gaggle != "web" || pins[0].Workflow != "flow" {
		t.Fatalf("UnmarkedRunPins = %+v, want the one started run", pins)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE control.run_pin SET terminal_at=now() WHERE instance_id=$1 AND run_id=$2`, store.instanceID, result.RunID); err != nil {
		t.Fatal(err)
	}
	if pins, err = store.UnmarkedRunPins(t.Context()); err != nil || len(pins) != 0 {
		t.Fatalf("after terminal: %+v, %v; want none", pins, err)
	}
}
