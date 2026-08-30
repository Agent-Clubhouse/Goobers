package engine

// Parity row E1-backlog-query-defaults — EXPECTED FAILURE.
//
// Inventory row: "backlog-query input defaulting: gaggle RequireLabels
// ([goobers:cloud] partition, MIRC-2) and self-identity assignedTo injected
// into every `goobers backlog-query` task that does not declare them."
// Runner site: internal/runner/run.go:4413-4414 (dispatchTask) over
// defaultBacklogQueryAssignedTo / defaultBacklogQueryRequireLabels
// (run.go:4328-4374). Engine: missing — engine.RunInput has no counterpart
// field and runTask never applies one.
//
// WHY IT MATTERS AND WHY A JOURNAL DIFF CANNOT SEE IT. Both journals say
// stage.finished success; the divergence lives entirely in the envelope the
// stage was handed. On the cloud instance the missing requireLabels default
// means an engine-driven backlog-curation run queries the WHOLE backlog
// instead of the [goobers:cloud] partition and claims items the local instance
// owns. This is exactly the class of gap the envelope surface exists for.
//
// The fixture is the real `query-backlog` stage from backlog-curation.yaml,
// which declares neither requireLabels nor assignedTo — so the defaulting
// predicate (a `goobers backlog-query` command whose inputs do not override)
// fires on the runner side for the production declaration, not a contrived one.
//
// Closed by plan item E1: RunInput gains BacklogQueryAssignedTo /
// BacklogQueryRequireLabels (pinned at start by both starters from
// selfIdentitiesByGaggle / requireLabelsByGaggle) and runTask applies them
// through the shared defaulting helpers. When it lands, DELETE this row's
// entry from parityExpectedFailures.

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
// wrong envelopes, and because this row is on parityExpectedFailures a graded
// assertion would be downgraded to a log line. Emptying
// Config.BacklogQueryRequireLabels left the whole suite green before this hook
// existed.
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
// parityExpectedFailures may legitimately grade: the engine does not apply the
// defaulting today. The explicit engine-side assertions come before the
// whole-surface diff so the failure names the missing inputs rather than the
// envelope divergence they cause.
func checkBacklogQueryDefaults(obs parityObservation) error {
	if err := requireEnvelopeInput(obs.Engine, "query-backlog", "requireLabels", parityRequireLabels); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	if err := requireEnvelopeInput(obs.Engine, "query-backlog", "assignedTo", parityAssignedTo); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}
