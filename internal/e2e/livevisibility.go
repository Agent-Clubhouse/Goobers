package e2e

import (
	"fmt"
	"time"
)

// LiveVisibilityObserver is S8's named observer (goobernetes-smoke.md §4
// S8): "a recorded portal/SSE observation (timestamped screenshot or SSE
// event capture) of a stage transition, with the run's terminal journal
// event timestamped LATER."
const LiveVisibilityObserver = "timestamped portal/SSE observation of a stage transition vs. the run's terminal journal.Event.Time"

// StageTransitionObservation is one captured sighting of a stage transition
// while a run was still in flight. §8 open point 3 leaves the CAPTURE FORM
// open ("SSE event log vs. timestamped portal screenshot; either satisfies
// the observer") — this type is form-agnostic on purpose: Source names
// which form a live procedure chose, and only the timestamp/stage identity
// matter to the assertion.
type StageTransitionObservation struct {
	// Source names the capture mechanism, e.g. "sse" or "portal-screenshot".
	// Free text: this package does not pick between the two forms S8 leaves
	// open.
	Source string
	Stage  string
	// ObservedAt is when the observation was captured — for an SSE
	// invalidation frame (apicontract.Invalidation, internal/httpapi/
	// eventstream.go — the frame itself carries no timestamp, so the
	// RECEIVING client's own capture time is the observation, matching how
	// a timestamped screenshot works too), the moment the frame was
	// received; for a screenshot, the moment it was taken.
	ObservedAt time.Time
}

// AssertLiveVisibility is S8: while a multi-stage run is in flight, the
// portal shows its stage transitions as they happen — before the run
// closes. Per-token streaming is explicitly not asserted (§4 S8's last
// sentence); this only checks ordering, never granularity.
//
// observations is every StageTransitionObservation captured during the
// procedure for one run; runTerminalAt is that run's terminal journal
// event's timestamp (journal.Event.Time on the run.finished/equivalent
// event). Terminal-only visibility — every observation landing at or after
// the terminal event — is an explicit fail: "live visibility is a v1
// functional requirement" (decision record D5).
func AssertLiveVisibility(observations []StageTransitionObservation, runTerminalAt time.Time) AssertionResult {
	if runTerminalAt.IsZero() {
		return invalid("no run terminal timestamp supplied", nil)
	}
	if len(observations) == 0 {
		return classify("", false, "no stage-transition observation was captured at all during the run", nil, observations)
	}

	var early []StageTransitionObservation
	for _, o := range observations {
		if o.ObservedAt.IsZero() {
			return invalid(fmt.Sprintf("observation for stage %q (%s) carries no timestamp", o.Stage, o.Source), o)
		}
		if o.ObservedAt.Before(runTerminalAt) {
			early = append(early, o)
		}
	}
	if len(early) == 0 {
		return classify("", false,
			fmt.Sprintf("every one of %d observation(s) arrived at or after the run's terminal event (%s) — this is today's closed-run projection shape, an explicit S8 fail", len(observations), runTerminalAt),
			nil, observations)
	}
	return classify("", true, "", early, nil)
}
