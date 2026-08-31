package engine

// Parity row E1-backlog-query-declared-inputs-win — must stay GREEN.
//
// The over-application half of the E1 contract, and the reason it is a row of
// its own rather than a line inside rowBacklogQueryDefaults: a port that
// closes E1 by stamping the gaggle's identity onto every `goobers
// backlog-query` stage would make the two failing rows green and silently
// retarget every lane that picks its own claim query. On the cloud instance
// that is the same partition bug in the opposite direction — implementation's
// query-backlog would stop claiming `goobers:ready` items and start claiming
// whatever the gaggle's RequireLabels names, under whatever identity the
// instance is configured with.
//
// rowLaneImplementation already pins the declared-requireLabels half on the
// SHIPPED lane. This row pins the pair, and specifically the half no
// production lane exercises: `assignedTo`. No shipped workflow declares
// assignedTo today (grep reference-workflows), which is precisely why a port
// could drop the override check on that input and have every lane row stay
// green. So the fixture is the real implementation lane stage with ONE
// declared input added, and both gaggle defaults configured to values that
// conflict with the declaration:
//
//	declared requireLabels goobers:ready   vs. gaggle default goobers:cloud
//	declared assignedTo    declared-claimer vs. self identity  goobersbot
//
// Both sides must hand the stage the DECLARED values. The defaulting is a full
// replace only when the task is silent — never a merge, never an override
// (internal/runner/run.go:4328-4374).
//
// It is green before E1 lands (the engine applies no defaulting at all, so it
// cannot over-apply one) and it must still be green after. Its failing-first
// character is against the PORT, not against today's engine: ablating the
// `if _, overridden := inputs[...]` guard in the shared defaulting turns this
// row red on both the runner and the engine sides.

import (
	"fmt"
	"testing"
)

// parityDeclaredAssignedTo is the fixture's declared claim identity,
// deliberately different from parityAssignedTo (the configured self identity
// this row runs with).
const parityDeclaredAssignedTo = "declared-claimer"

func init() {
	registerParityRow(parityCase{
		Row:  rowBacklogQueryDeclaredInputsWin,
		Name: "declared requireLabels and assignedTo survive both gaggle defaults",
		Lane: "implementation.yaml",
		// BOTH defaults configured, both conflicting with the declaration.
		BacklogQueryRequireLabels: parityRequireLabels,
		BacklogQueryAssignedTo:    parityAssignedTo,
		Build:                     buildBacklogQueryDeclaredInputsCase,
		Premise:                   premiseBacklogQueryDeclaredInputs,
		Check:                     checkBacklogQueryDeclaredInputs,
	})
}

func buildBacklogQueryDeclaredInputsCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := implementationLane(t)
	spec := laneChain(t, lane, "query-backlog")
	// The lane declares requireLabels itself; assignedTo is added here because
	// no shipped lane declares it. Everything else — command, trustLabel,
	// excludeLabels, maxItems, capabilities, policyActions — is the lane's.
	task := &spec.Tasks[0]
	if task.Inputs == nil {
		t.Fatalf("lane %q stage %q declares no inputs — the fixture would prove nothing", lane.Name, task.Name)
	}
	if got := task.Inputs["requireLabels"]; got != parityImplementationRequireLabels {
		t.Fatalf("lane %q stage %q declares requireLabels=%q, want %q — this row's premise is that the lane declares its own",
			lane.Name, task.Name, got, parityImplementationRequireLabels)
	}
	if _, declared := task.Inputs["assignedTo"]; declared {
		t.Fatalf("lane %q stage %q now declares assignedTo itself; drop the fixture's overlay", lane.Name, task.Name)
	}
	task.Inputs["assignedTo"] = parityDeclaredAssignedTo
	c.Spec = spec
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"query-backlog": {succeed(map[string]interface{}{"claimed-item": "42"})},
	}
}

// premiseBacklogQueryDeclaredInputs is the ungraded half: the RUNNER — the
// side that HAS the defaulting and is therefore the side that could over-apply
// it — leaves both declared inputs alone.
func premiseBacklogQueryDeclaredInputs(obs parityObservation) error {
	if err := requireDeclaredBacklogQueryInputs(obs.Runner); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

func checkBacklogQueryDeclaredInputs(obs parityObservation) error {
	if err := requireDeclaredBacklogQueryInputs(obs.Engine); err != nil {
		return errParityRow(obs.Case.Row, "%v", err)
	}
	return checkAllSurfaces(obs)
}

func requireDeclaredBacklogQueryInputs(side paritySide) error {
	if err := requireEnvelopeInput(side, "query-backlog", "requireLabels", parityImplementationRequireLabels); err != nil {
		return errDeclaredInputReplaced(err, "requireLabels", parityRequireLabels)
	}
	if err := requireEnvelopeInput(side, "query-backlog", "assignedTo", parityDeclaredAssignedTo); err != nil {
		return errDeclaredInputReplaced(err, "assignedTo", parityAssignedTo)
	}
	return nil
}

func errDeclaredInputReplaced(err error, input, configured string) error {
	return fmt.Errorf("%w — the stage DECLARES %s, so the gaggle default %q must not reach it: "+
		"the defaulting is a full replace only when the task is silent", err, input, configured)
}
