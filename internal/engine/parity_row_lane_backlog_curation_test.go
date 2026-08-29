package engine

// Parity row P0-lane-backlog-curation — the harness baseline.
//
// This is NOT an inventory gap. It walks the real backlog-curation lane
// (finding 002 R11: the first lane scheduled to move to the engine) end to end
// through both runners on the happy path, and it must stay GREEN. Its job is
// to prove the harness itself walks a production lane — envelopes, journal and
// terminal all agreeing — so that a row which DOES go red is credible evidence
// about that row rather than about the harness.

import "testing"

func init() {
	registerParityRow(parityCase{
		Row:  rowLaneBacklogCuration,
		Name: "whole lane on the happy path",
		Lane: "backlog-curation.yaml",
		// The spec and script are built lazily by Build because loading the
		// lane needs *testing.T; see parityCase.Spec population in
		// buildLaneCases.
		Build: buildBacklogCurationLaneCase,
	})
}

// buildBacklogCurationLaneCase walks every stage of the shipped lane in its
// declared order. Each deterministic stage reports the outputs its
// expectedOutputs contract names, so the walk reaches release-claim rather
// than short-circuiting.
func buildBacklogCurationLaneCase(t *testing.T, c *parityCase) {
	t.Helper()
	lane := backlogCurationLane(t)
	c.Spec = lane.Spec
	c.DSLVersion = lane.DSLVersion
	// The lane's stages declare no workspace, so both runners take the
	// default repo worktree — the local side needs the hermetic fixture repo.
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"reconcile-backlog":       {succeed(map[string]interface{}{"backlog-reconciliation": "0"})},
		"implementation-feedback": {succeed(map[string]interface{}{"implementation-feedback": "0"})},
		"sample-ready-pool":       {succeed(map[string]interface{}{"backlog-health": "ok"})},
		"query-backlog":           {succeed(map[string]interface{}{"claimed-items": "1"})},
		"surface-duplicates":      {succeed(map[string]interface{}{"dedupe-candidates": "0"})},
		"curate":                  {succeed(map[string]interface{}{"curated": "1"})},
		"release-claim":           {succeed(nil)},
	}
}
