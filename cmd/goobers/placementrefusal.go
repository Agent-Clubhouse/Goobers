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
// checkpoint decides the DAEMON's execution placement, so it solves against
// the runners the daemon's substrate can actually execute on
// (runnersolve.SolveExecutable: self only — the daemon runs stages
// in-process and cannot dispatch until #3482 moves it behind the dispatch
// seam). A workflow satisfiable only by remote runners VALIDATES clean —
// the config is fine, and an ENGINE-started run can place it via the #3588
// dispatch activity (bootstrap.PinStagePlacements solves the full
// inventory) — but a RUNNER-DRIVEN entry is refused here with a diagnostic
// naming where it could place, instead of green-lighting it on a remote
// runner's claims and then executing it on the daemon host that does not
// satisfy them.
//
// # Why the refusal is scoped to runner-driven entries (#3987)
//
// "The daemon's substrate executes this" stopped being true for every entry
// when #3876 made the engine/runner choice PER ENTRY. The reasoning above
// holds exactly as far as the entry whose stages this daemon runs in-process;
// applied to an entry selectEngineForEntry routes to the engine, it refused a
// lane on behalf of a substrate that lane never touches. That is not a
// theoretical gap: it took sixteen workflows off the air on the live
// instance — backlog-curation and defect-nomination among them — the moment
// they declared runsOn, because a pod-shaped requirement excludes self by
// construction (runnersolve.go:184, "Self enforces NOTHING implicitly") and
// so is unsatisfiable on a self-only substrate BY DEFINITION.
//
// An engine-selected entry has already cleared the stronger bar: to be
// selected at all, bootstrap.PinStagePlacements — the same function that pins
// the run's StartSpec — must have placed EVERY one of its stages on the full
// declared inventory, with no stage landing on self. Refusing it here would
// refuse a lane whose placement is already proven, using a solve that models
// a dispatcher it does not use.
//
// The carve-out is therefore narrow by construction, and fails closed at
// every edge: an engine-disabled instance, a self-pinned or mixed lane, a
// lane whose full-inventory pin failed, and a caller that supplies no
// selections at all each keep the refusal. The exemption is only ever granted
// against a proven engineSelection.UseEngine, never inferred from the
// presence of runsOn.
//
// Only a declared runners: inventory produces boot refusals. A
// zero-declaration instance solves nothing here: its capability union check
// has the same outcome delivered per-run at dispatch (the self-runner check
// the solver also serves), byte-identical journals and diagnostics with
// every previous release — the goobernetes-architecture.md §11 item 1
// invariance guard.

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

// placementDecisions is checkpoint 3's per-workflow outcome, split by what
// the daemon is entitled to conclude from a self-only solve.
//
// Two maps rather than one map plus a predicate: an exempted lane's
// diagnostic is real information (it names the runners the stage COULD have
// placed on), and the daemon logs it. Discarding it would leave an operator
// debugging an engine-side dispatch problem with no record that the lane is
// unplaceable locally; folding it into Refusals would re-create the outage.
type placementDecisions struct {
	// Refusals maps a workflow to the diagnostic that refuses it. Its value
	// is what daemon.go stamps onto WorkflowEntry.PlacementRefusal, so an
	// entry absent here serves normally.
	Refusals map[localscheduler.WorkflowIdentity]string
	// EngineDeferred maps an ENGINE-SELECTED workflow that the daemon's own
	// substrate cannot place to the diagnostic that would have refused it.
	// These lanes serve: the engine places them on the full declared
	// inventory (#3987). Reported, never enforced.
	EngineDeferred map[localscheduler.WorkflowIdentity]string
}

// placementRefusals solves every compiled workflow against the declared
// inventory and returns the refusal diagnostic per unplaceable workflow.
// Empty whenever the instance declares no runners: inventory.
//
// selections is the per-entry engine/runner decision (engineSelections),
// computed first and passed in — the daemon runs the two passes in that
// order. The dependency is one-way by construction and cannot cycle:
// engineSelections reads only cfg, set and the compiled machines, and never
// consults a refusal. A missing or nil selection means "not proven
// engine-selected" and therefore refuses, which is what makes every caller
// that has not done the engine pass fail closed.
func placementRefusals(
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	machines map[localscheduler.WorkflowIdentity]*workflow.Machine,
	selections map[localscheduler.WorkflowIdentity]engineSelection,
) (placementDecisions, error) {
	decisions := placementDecisions{
		Refusals:       make(map[localscheduler.WorkflowIdentity]string),
		EngineDeferred: make(map[localscheduler.WorkflowIdentity]string),
	}
	if cfg == nil || len(cfg.Runners) == 0 {
		return decisions, nil
	}
	inventory := runnersolve.Inventory{Runners: cfg.PlacementRunners(runnersolve.HostOS())}
	gaggleSpecs := make(map[string]apiv1.GaggleSpec, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleSpecs[set.Gaggles[i].Name] = set.Gaggles[i].Spec
	}
	for identity, machine := range machines {
		requirements, err := workflow.StagePlacements(machine.Def, gaggleSpecs[identity.Gaggle], goobers)
		if err != nil {
			// The machine compiled, so its interpreter resolves; surface the
			// impossible rather than silently skipping the solve.
			return placementDecisions{}, fmt.Errorf("workflow %q placement requirements: %w", identity.Workflow, err)
		}
		unsatisfiable := runnersolve.SolveExecutable(inventory, requirements).Unsatisfiable()
		if len(unsatisfiable) == 0 {
			continue
		}
		parts := make([]string, 0, len(unsatisfiable))
		for _, stage := range unsatisfiable {
			parts = append(parts, stage.Unsat.Diagnostic)
		}
		diagnostic := strings.Join(parts, "; ")
		// The whole of the #3987 carve-out. The solve above answers "can the
		// DAEMON execute this in-process?"; for an engine-selected lane that
		// is not the question the daemon acts on, because it does not execute
		// that lane at all.
		if selections[identity].UseEngine {
			decisions.EngineDeferred[identity] = diagnostic
			continue
		}
		decisions.Refusals[identity] = diagnostic
	}
	return decisions, nil
}
