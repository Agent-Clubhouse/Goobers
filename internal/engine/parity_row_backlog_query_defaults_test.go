package engine

// Parity row E1-backlog-query-defaults — CLOSED by plan item E1 (#3873); must
// stay GREEN.
//
// Inventory row: "backlog-query input defaulting: gaggle RequireLabels
// ([goobers:cloud] partition, MIRC-2) and self-identity assignedTo injected
// into every `goobers backlog-query` task that does not declare them."
// Runner site: internal/runner/run.go:4413-4414 (dispatchTask) over
// defaultBacklogQueryAssignedTo / defaultBacklogQueryRequireLabels
// (run.go:4328-4374). Engine site, as of E1: runTask applies
// internal/backlogdefaults.Apply over RunInput.BacklogQueryAssignedTo /
// BacklogQueryRequireLabels, which Registry.StartInputVersion pins from
// StartSpec.
//
// WHY IT MATTERS AND WHY A JOURNAL DIFF CANNOT SEE IT. Both journals say
// stage.finished success; the divergence lives entirely in the envelope the
// stage was handed. On the cloud instance a missing requireLabels default
// means an engine-driven backlog-curation run queries the WHOLE backlog
// instead of the [goobers:cloud] partition and claims items the local instance
// owns. This is exactly the class of gap the envelope surface exists for.
//
// The fixture is the real `query-backlog` stage from backlog-curation.yaml,
// which declares neither requireLabels nor assignedTo — so the defaulting
// predicate (a `goobers backlog-query` command whose inputs do not override)
// fires for the production declaration, not a contrived one.
//
// This row is the INPUT-EQUALITY half of the contract. Its two siblings hold
// the other two halves and must be read together:
// rowBacklogQueryClaimPartition (the sibling instance's item stays
// unclaimable) and rowBacklogQueryDeclaredInputsWin (a stage that declares its
// own claim query keeps it).

import "testing"

const (
	parityRequireLabels = "goobers:cloud"
	parityAssignedTo    = "goobersbot"
)

func init() {
	registerParityRow(parityCase{
		Row:                       rowBacklogQueryDefaults,
		Name:                      "gaggle requireLabels and self-identity reach the stage envelope",
		Lane:                      "backlog-curation.yaml",
		Build:                     buildBacklogQueryDefaultsCase,
		BacklogQueryRequireLabels: parityRequireLabels,
		BacklogQueryAssignedTo:    parityAssignedTo,
		Premise:                   premiseBacklogQueryDefaults,
		Check:                     checkBacklogQueryDefaults,
	})
}

func buildBacklogQueryDefaultsCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := backlogCurationLane(t)
	// query-backlog alone: the row is about what that ONE stage is handed.
	c.Spec = laneChain(t, lane, "query-backlog")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"query-backlog": {succeed(map[string]interface{}{"claimed-items": "1"})},
	}
}

// premiseBacklogQueryDefaults asserts the RUNNER really applies the defaulting.
// It runs ungraded, which is what stops the row from passing vacuously if the
// runner ever loses it: a diff alone would report "identical" for two equally
// wrong envelopes. Emptying Config.BacklogQueryRequireLabels left the whole
// suite green before this hook existed, and now that the engine copy exists
// (internal/backlogdefaults) the premise is also the drift guard between the
// two copies — a runner that stops defaulting fails here rather than being
// silently matched by an engine that still does.
func premiseBacklogQueryDefaults(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Runner, "query-backlog", "requireLabels", parityRequireLabels); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	if err := requireEnvelopeInput(obs.Runner, "query-backlog", "assignedTo", parityAssignedTo); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

// checkBacklogQueryDefaults is the DIVERGENCE half — the part
// parityExpectedFailures may grade at all: does the engine apply the same
// defaulting? The explicit engine-side assertions come before the whole-surface
// diff so a regression names the missing inputs rather than the envelope
// divergence they cause.
func checkBacklogQueryDefaults(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Engine, "query-backlog", "requireLabels", parityRequireLabels); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := requireEnvelopeInput(obs.Engine, "query-backlog", "assignedTo", parityAssignedTo); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}
