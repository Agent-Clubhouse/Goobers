package engine

// Parity row E3-placement-provenance.
//
// Inventory row: "runner.placement provenance" (finding 002 parity inventory;
// decision 005 plan item E3, filed as #3875). Runner site:
// internal/runner/run.go's runTask, which appends journal.PlacementEvent beside
// every attempt's stage.started once the deployment has declared placement at
// all (recordsPlacement — goobernetes-architecture.md §11 item 1). Engine:
// ABSENT — DispatchStageResult.Placement has carried a pod attempt's substrate
// provenance since the dispatch seam landed and has never had a journal call
// site, and the in-process arms report nothing at all. The consequence is
// §11 acceptance 6 with no evidence behind it: an engine-driven run's
// events.jsonl cannot say which runner served a stage, which pod carried it,
// which image that pod ran, or how long the attempt waited for capacity.
//
// INVISIBLE TO THE DEFAULT SURFACES, which is why this row carries its own
// Check. runner.placement is conformance-EXCLUDED (internal/journal/event.go's
// IsConformanceNormative), so diffConformanceViews reports nothing; it changes
// no envelope and no terminal. The row therefore compares the RAW journals for
// the runner.* provenance events themselves.
//
// The fixture declares placement through the environment family the local
// runner already gates on (GOOBERS_RUNNER_NODE / _POD / _IMAGE), because a row
// that left it undeclared would compare "no placement" against "no placement"
// and pass on a zero-declaration install — which is a real and REQUIRED
// behaviour, pinned separately by
// TestEnginePlacementProvenanceRespectsZeroDeclaration.
//
// Closed by plan item E3: dispatchWithRetry journals runner.placement from the
// dispatch result — DispatchStageResult.Placement for a pod attempt,
// DispatchStageResult.SelfPlacement (runner.SelfPlacement, computed in the
// activity so the fact reaches the workflow through history and replays) for an
// in-process one. Then DELETE this row's parityExpectedFailures entry.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func init() {
	registerParityRow(parityCase{
		Row:     rowPlacementProvenance,
		Name:    "every attempt journals where it executed",
		Lane:    "backlog-curation.yaml",
		Build:   buildPlacementProvenanceCase,
		Premise: premisePlacementProvenance,
		Check:   checkPlacementProvenance,
	})
}

// placementParityNode is the declared node identity both sides must report. It
// is a value neither side could invent, so a placement event carrying it proves
// the deployment's declaration reached the journal rather than some default.
const placementParityNode = "parity-node-e3"

func buildPlacementProvenanceCase(t *testing.T, c *parityCase) {
	t.Helper()
	// Declaring placement is what turns provenance recording on, on BOTH sides
	// and through the same gate (runner.placementDeclared). Set here rather
	// than in the harness so every other row keeps running on an undeclared
	// install and keeps proving zero-declaration invariance by default.
	t.Setenv(runner.EnvPlacementNode, placementParityNode)

	lane := backlogCurationLane(t)
	c.Spec = laneChain(t, lane, "reconcile-backlog", "implementation-feedback")
	c.DSLVersion = lane.DSLVersion
	c.UsesRepo = true
	c.Script = map[string][]scriptedCall{
		"reconcile-backlog":       {succeed(map[string]interface{}{"backlog-reconciliation": "1"})},
		"implementation-feedback": {succeed(map[string]interface{}{"implementation-feedback": "1"})},
	}
}

// premisePlacementProvenance pins the RUNNER's own behaviour: it still journals
// one runner.placement per attempt, naming the declared node, immediately after
// that attempt's stage.started.
//
// Ungraded, and that matters more here than for most rows: this row is on the
// expected-failure list while E3 is open, so a graded assertion would let
// somebody delete runTask's PlacementEvent append and still see this suite
// green — with the row logging "expected failure, still open" as it went.
func premisePlacementProvenance(obs parityObservation) error {
	placements := parityPlacements(obs.Runner)
	if len(placements) == 0 {
		return errParityPremisef(obs.Case.Row,
			"runner journaled no %s events for a placement-declaring deployment — internal/runner's per-attempt provenance is the behaviour this row ports",
			journal.EventRunnerPlacement)
	}
	if err := requireStagesDispatched(obs.Runner, []string{"reconcile-backlog", "implementation-feedback"}); err != nil {
		return errParityPremisef(obs.Case.Row, "%v — the fixture no longer walks two stages", err)
	}
	for _, key := range sortedPlacementKeys(placements) {
		p := placements[key]
		if p.Runner != journal.PlacementRunnerSelf {
			return errParityPremisef(obs.Case.Row,
				"runner placement for %s names runner %q, want %q", key, p.Runner, journal.PlacementRunnerSelf)
		}
		if p.Node != placementParityNode {
			return errParityPremisef(obs.Case.Row,
				"runner placement for %s reports node %q, want the declared %q — the deployment's declaration is no longer reaching the journal",
				key, p.Node, placementParityNode)
		}
	}
	if err := requirePlacementFollowsEveryStageStart(obs.Runner); err != nil {
		return errParityPremisef(obs.Case.Row, "%v", err)
	}
	return nil
}

// checkPlacementProvenance is the row's graded divergence: the two sides must
// journal the same provenance for the same attempts, and the engine must place
// it where a reader correlating it to an attempt can find it.
//
// The default surfaces are checked too — a port that journals provenance by
// changing what a stage receives, or what the run terminates as, is not the
// port this row asks for.
func checkPlacementProvenance(obs parityObservation) error {
	if err := checkAllSurfaces(obs); err != nil {
		return err
	}
	runnerPlacements, enginePlacements := parityPlacements(obs.Runner), parityPlacements(obs.Engine)
	if len(enginePlacements) == 0 {
		return fmt.Errorf("engine journaled no %s events; the runner journaled %d (%s). "+
			"DispatchStageResult.Placement exists but has no journal call site, and the in-process arms report no placement at all",
			journal.EventRunnerPlacement, len(runnerPlacements), strings.Join(sortedPlacementKeys(runnerPlacements), " "))
	}
	for _, key := range sortedPlacementKeys(runnerPlacements) {
		got, ok := enginePlacements[key]
		if !ok {
			return fmt.Errorf("engine journaled no %s for %s (it journaled %s)",
				journal.EventRunnerPlacement, key, strings.Join(sortedPlacementKeys(enginePlacements), " "))
		}
		if want := formatParityPlacement(runnerPlacements[key]); formatParityPlacement(got) != want {
			return fmt.Errorf("placement provenance for %s diverges:\n  runner: %s\n  engine: %s",
				key, want, formatParityPlacement(got))
		}
	}
	for _, key := range sortedPlacementKeys(enginePlacements) {
		if _, ok := runnerPlacements[key]; !ok {
			return fmt.Errorf("engine journaled a %s for %s that the runner did not", journal.EventRunnerPlacement, key)
		}
	}
	return requirePlacementFollowsEveryStageStart(obs.Engine)
}

// parityPlacements indexes one side's decoded placement payloads by
// "<stage>#<attempt>".
func parityPlacements(side paritySide) map[string]journal.Placement {
	out := map[string]journal.Placement{}
	for _, event := range side.Events {
		if placement, ok := journal.PlacementFromEvent(event); ok {
			out[fmt.Sprintf("%s#%d", event.Stage, event.Attempt)] = placement
		}
	}
	return out
}

func sortedPlacementKeys(placements map[string]journal.Placement) []string {
	keys := make([]string, 0, len(placements))
	for key := range placements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// formatParityPlacement renders the compared payload. Every field is printed,
// for the reason parityEnvelope.String documents: a comparison that hides a
// field it compares produces the worst possible failure message.
func formatParityPlacement(p journal.Placement) string {
	queued, podStarted := "-", "-"
	if p.QueuedAt != nil {
		queued = "set"
	}
	if p.PodStartedAt != nil {
		podStarted = "set"
	}
	return fmt.Sprintf("runner=%q node=%q host=%q os=%q image=%q pod=%q queuedAt=%s podStartedAt=%s",
		p.Runner, p.Node, p.Host, p.OS, p.Image, p.Pod, queued, podStarted)
}

// requirePlacementFollowsEveryStageStart is the far-side evidence shape stated
// in #3875 — "every stage.started of the run is followed by a runner.placement
// event" — asserted on the wire order of one side's events.jsonl.
//
// Between one stage.started and the next, not immediately after it: a pod
// attempt's placement is not known until the attempt settles, so the engine
// journals it once the dispatch returns. What a reader needs is that the
// provenance is unambiguously attributable to the attempt it follows, and the
// attempt's own window is what makes that true.
func requirePlacementFollowsEveryStageStart(side paritySide) error {
	type pending struct {
		stage   string
		attempt int
		found   bool
	}
	var open *pending
	var unmatched []string
	close := func() {
		if open != nil && !open.found {
			unmatched = append(unmatched, fmt.Sprintf("%s#%d", open.stage, open.attempt))
		}
	}
	for _, event := range side.Events {
		switch event.Type {
		case journal.EventStageStarted:
			close()
			open = &pending{stage: event.Stage, attempt: event.Attempt}
		case journal.EventRunnerPlacement:
			if open != nil && event.Stage == open.stage && event.Attempt == open.attempt {
				open.found = true
			}
		}
	}
	close()
	if len(unmatched) > 0 {
		return fmt.Errorf("%s side: %d stage.started event(s) are not followed by a %s for the same attempt: %s",
			side.Name, len(unmatched), journal.EventRunnerPlacement, strings.Join(unmatched, " "))
	}
	return nil
}
