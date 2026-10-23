package engine

// Parity row P0-lane-implementation — must stay GREEN.
//
// Two jobs, both baselines rather than gaps.
//
// (1) SECOND LANE. implementation is the second lane scheduled to move to the
// engine (finding 002 R11). Its deterministic prefix walks here so a port that
// breaks the second lane is caught by this suite and not by the cutover.
// The lane's later stages (implement, the four gates, ci-poll) need the gate
// engine half (#3858) and agentic-review evidence before they can be walked on
// both runners; they arrive as their own rows with the E4-E8 ports.
//
// (2) DECLARED WINS. implementation's query-backlog DECLARES
// `requireLabels: goobers:ready` (reference-workflows/.../implementation.yaml).
// backlog-curation's does not — which is why the E1 row uses that one. So this
// row runs with a gaggle RequireLabels default CONFIGURED and asserts the
// declared value survives untouched on both sides: the defaulting is a full
// replace only when the task is silent, never a merge and never an override
// (internal/runner/run.go:4349-4356). That is the half of the E1 contract a
// port could most easily get wrong — blanket-stamping the gaggle default over
// every backlog-query stage would silently retarget this lane's claim query,
// and this row is what says no.
//
// assignedTo is deliberately NOT configured here: the lane does not declare it,
// so configuring it would reproduce the E1 gap and turn this baseline red for a
// reason that already has its own row.

import "testing"

// parityImplementationRequireLabels is the value implementation.yaml's
// query-backlog declares. laneTask fails loudly if the stage disappears; this
// constant is the second tripwire, for the input itself.
const parityImplementationRequireLabels = "goobers:ready"

func init() {
	registerParityRow(parityCase{
		Row:  rowLaneImplementation,
		Name: "declared requireLabels survives a configured gaggle default",
		Lane: "implementation.yaml",
		// A gaggle default is configured; the lane declares its own, so
		// neither runner may apply it.
		BacklogQueryRequireLabels: parityRequireLabels,
		Build:                     buildImplementationLaneCase,
		Premise:                   premiseImplementationLane,
		Check:                     checkImplementationLane,
	})
}

func buildImplementationLaneCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := implementationLane(t)
	c.Spec = laneChain(t, lane, "query-backlog", "gather-implement-context")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"query-backlog":            {succeed(map[string]interface{}{"claimed-item": "42"})},
		"gather-implement-context": {succeed(map[string]interface{}{"implementation-context": "ok"})},
	}
}

// premiseImplementationLane pins the DECLARED-WINS half on the runner: the
// lane's own requireLabels reaches the envelope untouched even though a
// conflicting gaggle default is configured. This row is green, so without the
// premise a runner that stopped dispatching query-backlog at all — or a lane
// that stopped declaring the value — would leave the row green and meaningless.
func premiseImplementationLane(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Runner, "query-backlog", "requireLabels", parityImplementationRequireLabels); err != nil {
		return errParityPremisef(obs.Case.Row,
			"%v — a declared requireLabels must win over the gaggle default, never be replaced by it", err)
	}
	return nil
}

func checkImplementationLane(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Engine, "query-backlog", "requireLabels", parityImplementationRequireLabels); err != nil {
		return errParityRow(obs.Case.Row,
			"%v — a declared requireLabels must win over the gaggle default, never be replaced by it", err)
	}
	return checkAllSurfaces(obs)
}
