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
// Unlike checkpoint 1 (config validity — whole declared inventory), this
// checkpoint decides EXECUTION placement, so it solves against the runners
// the current substrate can actually execute on
// (runnersolve.SolveExecutable: self only, until the #3513 dispatcher
// exists). A workflow satisfiable only by remote runners VALIDATES clean —
// the config is fine — but is refused here with a diagnostic naming where
// it could place and pointing at #3513, instead of being green-lit on a
// remote runner's claims and then executed on the daemon host that does not
// satisfy them.
//
// Only a declared runners: inventory produces boot refusals. A
// zero-declaration instance solves nothing here: its capability union check
// has the same outcome delivered per-run at dispatch (the self-runner check
// the solver also serves), byte-identical journals and diagnostics with
// every previous release — the goobernetes-architecture.md §11 item 1
// invariance guard.

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

// placementRefusals solves every compiled workflow against the declared
// inventory and returns the refusal diagnostic per unplaceable workflow.
// Empty (nil) whenever the instance declares no runners: inventory.
func placementRefusals(
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	machines map[localscheduler.WorkflowIdentity]*workflow.Machine,
) (map[localscheduler.WorkflowIdentity]string, error) {
	if cfg == nil || len(cfg.Runners) == 0 {
		return nil, nil
	}
	inventory := runnersolve.Inventory{Runners: cfg.PlacementRunners(runnersolve.HostOS())}
	gaggleSpecs := make(map[string]apiv1.GaggleSpec, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleSpecs[set.Gaggles[i].Name] = set.Gaggles[i].Spec
	}
	refusals := make(map[localscheduler.WorkflowIdentity]string)
	for identity, machine := range machines {
		requirements, err := workflow.StagePlacements(machine.Def, gaggleSpecs[identity.Gaggle], goobers)
		if err != nil {
			// The machine compiled, so its interpreter resolves; surface the
			// impossible rather than silently skipping the solve.
			return nil, fmt.Errorf("workflow %q placement requirements: %w", identity.Workflow, err)
		}
		unsatisfiable := runnersolve.SolveExecutable(inventory, requirements).Unsatisfiable()
		if len(unsatisfiable) == 0 {
			continue
		}
		parts := make([]string, 0, len(unsatisfiable))
		for _, stage := range unsatisfiable {
			parts = append(parts, stage.Unsat.Diagnostic)
		}
		refusals[identity] = strings.Join(parts, "; ")
	}
	return refusals, nil
}
