package main

// placementrefusal.go is checkpoint 3 of the three-checkpoint admission
// (dsl-3.0.md §5, decision record D4; #2860's "refusing one run is
// proportionate; refusing to start is not"): at daemon start the shared
// constraint solver (internal/runnersolve — the same implementation
// `goobers validate` runs at checkpoint 1) is applied per workflow against
// the pinned runner inventory, and an unsatisfiable workflow is MARKED
// REFUSED with a named diagnostic instead of anything being fatal. The
// daemon starts, every other workflow serves, the refusal is journaled
// (workflow.refused, surfaced by `goobers status`), and the scheduler
// refuses that workflow's runs (localscheduler.ReasonPlacementUnsatisfiable).
//
// Unlike checkpoint 1 (config validity against the whole declared inventory),
// this checkpoint solves against the local daemon substrate. Contained child
// stages are excluded after their separate authenticated placement check; all
// remaining stages must be executable by the local runner.

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

// sortedWorkflowIdentities orders a diagnostic map's keys so the daemon's
// startup report is byte-stable across boots of the same config. Ranging a
// map directly would reorder the lines on every start, which is exactly the
// noise that makes a boot-log diff useless for spotting a real change.
func sortedWorkflowIdentities(diagnostics map[localscheduler.WorkflowIdentity]string) []localscheduler.WorkflowIdentity {
	identities := make([]localscheduler.WorkflowIdentity, 0, len(diagnostics))
	for identity := range diagnostics {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Gaggle != identities[j].Gaggle {
			return identities[i].Gaggle < identities[j].Gaggle
		}
		return identities[i].Workflow < identities[j].Workflow
	})
	return identities
}

// placementDecisions is checkpoint 3's per-workflow local outcome.
type placementDecisions struct {
	// Refusals maps a workflow to the diagnostic that refuses it. Its value
	// is what daemon.go stamps onto WorkflowEntry.PlacementRefusal, so an
	// entry absent here serves normally.
	Refusals map[localscheduler.WorkflowIdentity]string
}

// placementRefusals solves every compiled workflow against the declared
// inventory and returns the refusal diagnostic per unplaceable workflow.
// Empty whenever the instance declares no runners: inventory.
//
// placements identifies authenticated contained-child stages that do not run
// on the daemon host.
func placementRefusals(
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	machines map[localscheduler.WorkflowIdentity]*workflow.Machine,
	placements map[localscheduler.WorkflowIdentity]localPlacement,
) (placementDecisions, error) {
	decisions := placementDecisions{
		Refusals: make(map[localscheduler.WorkflowIdentity]string),
	}
	if cfg == nil || (len(cfg.Runners) == 0 && !cfg.HasIsolationMandates() && !cfg.SelfExecutionDenied()) {
		return decisions, nil
	}
	inventory := cfg.PlacementInventory(runnersolve.HostOS())
	gaggleSpecs := make(map[string]apiv1.GaggleSpec, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleSpecs[set.Gaggles[i].Name] = set.Gaggles[i].Spec
	}
	for identity, machine := range machines {
		requirements, err := workflow.IsolationStagePlacements(machine.Def, gaggleSpecs[identity.Gaggle], goobers, inventory.ClassMandates, inventory.SelfExecutionDenied)
		if err != nil {
			// The machine compiled, so its interpreter resolves; surface the
			// impossible rather than silently skipping the solve.
			return placementDecisions{}, fmt.Errorf("workflow %q placement requirements: %w", identity.Workflow, err)
		}
		requirements = placements[identity].hostStageRequirements(requirements)
		unsatisfiable := runnersolve.SolveExecutable(inventory, requirements).Unsatisfiable()
		if len(unsatisfiable) == 0 {
			continue
		}
		parts := make([]string, 0, len(unsatisfiable))
		for _, stage := range unsatisfiable {
			parts = append(parts, stage.Unsat.Diagnostic)
		}
		diagnostic := strings.Join(parts, "; ")
		decisions.Refusals[identity] = diagnostic
	}
	return decisions, nil
}
