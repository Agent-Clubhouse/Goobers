//go:build cloudintegration

package noworkstreakpg_test

import (
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/cloud/noworkstreakpg"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/providers"
)

var repo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

func TestReplayedRunsAreRecognisedWithinTheCapAndForgottenBeyondIt(t *testing.T) {
	pool := pgtest.Pool(t, "nwpg")
	controller := pgtest.ControllerPool(t, pool)
	manager := pgtest.ControllerManager(t, pool, "leader", 0)
	handle := pgtest.Acquire(t, manager)
	store := noworkstreakpg.Store{Fence: manager}
	terminal := nowork.Terminal{Stage: "implement", Reason: "nothing to do"}
	total := noworkstreakpg.MaxRecentRuns + 4
	for i := 0; i < total; i++ {
		next, _, replayed, err := store.Increment(t.Context(), handle, repo, "42", fmt.Sprintf("r%d", i), terminal)
		if err != nil || replayed || next.Count != i+1 {
			t.Fatalf("run r%d: count %d replayed %v err %v", i, next.Count, replayed, err)
		}
	}
	var length int
	if err := controller.QueryRow(t.Context(), `SELECT cardinality(recent_runs) FROM control.no_work_streak`).Scan(&length); err != nil || length != noworkstreakpg.MaxRecentRuns {
		t.Fatalf("remembered %d runs (%v), want the cap of %d", length, err, noworkstreakpg.MaxRecentRuns)
	}
	// The newest run is a replay (the update path remembered it)...
	next, previous, replayed, err := store.Increment(t.Context(), handle, repo, "42", fmt.Sprintf("r%d", total-1), terminal)
	if err != nil || !replayed || next.Count != total || previous.Count != total {
		t.Fatalf("replay of the newest run: next %d previous %d replayed %v err %v", next.Count, previous.Count, replayed, err)
	}
	// ...and a run older than the memory is not.
	if _, _, replayed, err := store.Increment(t.Context(), handle, repo, "42", "r0", terminal); err != nil || replayed {
		t.Fatalf("a run beyond the memory was treated as a replay: %v %v", replayed, err)
	}
}
