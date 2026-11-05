package runsemantics

import (
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/gate"
)

// The #3929 ruling in one table: a learning episode is injected IFF the gate
// result's repass attempt is at least 1. Nothing else — not the failure class,
// not the target's identity, not whether the target happens to be upstream in
// some independently re-derived sense — participates.
//
// This is the single predicate BOTH drivers call (internal/engine's gate arm
// and this package's stepGate), which is the whole point of hoisting it here:
// the engine used to re-derive "does this gate send work back?" from its own
// upstream map, and a second derivation is a second thing to drift.
func TestLearningEpisodeAppliesToRepassIsTheRuling(t *testing.T) {
	for _, tt := range []struct {
		attempt int
		want    bool
		why     string
	}{
		{attempt: 0, want: false, why: "a forward branch: the gate routed to a stage that has not run, so there is nothing to correct"},
		{attempt: 1, want: true, why: "the first true repass"},
		{attempt: 2, want: true, why: "a later repass, still within the budget"},
		{attempt: gate.DefaultMaxRepasses, want: true, why: "the last repass the policy budget allows"},
		{attempt: -1, want: false, why: "never produced by trackRepass, but must not be read as a repass if it ever were"},
	} {
		t.Run(fmt.Sprintf("attempt=%d", tt.attempt), func(t *testing.T) {
			if got := LearningEpisodeAppliesToRepass(tt.attempt); got != tt.want {
				t.Fatalf("LearningEpisodeAppliesToRepass(%d) = %v, want %v — %s", tt.attempt, got, tt.want, tt.why)
			}
		})
	}
}
