package bootstrap

// placement.go is the run-start half of the #3588 mode-3 cutover: it resolves
// every task's execution placement ONCE, before the workflow starts, and pins
// the outcome into engine.RunInput — the same WF-016 snapshot discipline that
// pins the definition. The workflow then reads placement as data
// (engine.PinnedPlacement); it never solves, never reads instance.Config, and
// never touches Kubernetes, which is what keeps it replay-deterministic
// (architecture D8).
//
// The solve here is the full-inventory Solve, not SolveExecutable, and the
// difference is the cutover itself: SolveExecutable's self-only substrate
// encodes "no dispatcher can reach a remote runner", which stopped being true
// for ENGINE-started runs when the dispatch activity landed — an
// engine-started stage can now execute on every DECLARED runner (self through
// the local arms, image/deployment hosts through DispatchStage). The daemon's
// own local execution path still cannot dispatch, so its boot/admission
// checkpoints (placementRefusals, localscheduler) deliberately keep the
// self-only substrate until #3482 moves the daemon behind this same seam.

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/workflow"
)

// ledgerTouchingActions is the claims-ledger policy-action vocabulary: a
// stage declaring any of these mutates instance-ledger state (claims,
// close-out, release) and therefore NEVER places on Windows
// (goobernetes-architecture.md D12 — no path to the RWO instance root). The
// DSL guarantees completeness of the declared set: a command that prescribes
// one of these actions without declaring it fails validation
// (policyActionProblems), so reading Task.PolicyActions reads the whole
// truth for any compiled workflow.
var ledgerTouchingActions = map[string]struct{}{
	"claim-backlog-items":   {},
	"release-backlog-claim": {},
	"release-pr-claim":      {},
}

func taskLedgerTouching(task apiv1.Task) bool {
	for _, action := range task.PolicyActions {
		if _, ok := ledgerTouchingActions[action]; ok {
			return true
		}
	}
	return false
}

// PinStagePlacements resolves each task's execution placement for one
// engine-started run of def and returns the pinned list for
// engine.StartSpec.Placements.
//
// Zero-declaration invariance (architecture §11 item 1) is decided here, by
// inventory shape alone: with no runners: block, or an inventory whose every
// entry is self (LocalMode), it returns nil and RunInput carries no
// placements — the workflow's every arm then behaves byte-identically to
// before the cutover. Only a declared non-self runner produces pins.
//
// An unsatisfiable stage refuses the START with the solver's named
// diagnostic: green-lighting a run whose stage can never place would strand
// it at schedule-to-start instead of telling the operator why.
func PinStagePlacements(cfg *instance.Config, set *instance.ConfigSet, gaggle string, def workflow.Definition) ([]engine.PinnedPlacement, error) {
	if cfg == nil || len(cfg.Runners) == 0 {
		return nil, nil
	}
	inventory := runnersolve.Inventory{Runners: cfg.PlacementRunners(runnersolve.HostOS())}
	if inventory.LocalMode() {
		return nil, nil
	}

	var gaggleSpec apiv1.GaggleSpec
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			gaggleSpec = set.Gaggles[i].Spec
			break
		}
	}
	goobers := make(map[string]apiv1.GooberSpec, len(set.Goobers))
	for i := range set.Goobers {
		goobers[set.Goobers[i].Name] = set.Goobers[i].Spec
	}
	requirements, err := workflow.StagePlacements(def, gaggleSpec, goobers)
	if err != nil {
		return nil, fmt.Errorf("workflow %q placement requirements: %w", def.Name, err)
	}

	specs := make(map[string]dispatcher.RunnerSpec)
	for _, entry := range cfg.ResolvedRunners() {
		spec, serr := dispatcher.SpecFromEntry(entry)
		if serr != nil {
			return nil, serr
		}
		specs[spec.Name] = spec
	}

	result := runnersolve.Solve(inventory, requirements)
	placements := make([]engine.PinnedPlacement, 0, len(result.Stages))
	for i, stage := range result.Stages {
		if stage.Unsat != nil {
			return nil, fmt.Errorf("workflow %q stage %q cannot place on the declared runners: inventory: %s", def.Name, stage.Stage, stage.Unsat.Diagnostic)
		}
		// Solve and StagePlacements both walk def.Spec.Tasks in order, so
		// index i names the same task across all three.
		req := requirements[i]
		ledger := taskLedgerTouching(def.Spec.Tasks[i])
		eligible := make([]dispatcher.RunnerSpec, 0, len(stage.Eligible))
		for _, name := range stage.Eligible {
			spec, ok := specs[name]
			if !ok {
				return nil, fmt.Errorf("workflow %q stage %q: solver named runner %q which the resolved inventory does not contain", def.Name, stage.Stage, name)
			}
			eligible = append(eligible, spec)
		}
		// SelectRunner is the SAME pure function the dispatcher applies at
		// dispatch time (Linux-preferring, Windows excluded for
		// ledger-touching stages), so the queue pinned here and the runner
		// resolved there cannot disagree.
		selected, serr := dispatcher.SelectRunner(dispatcher.Attempt{
			RunID: def.Name, Stage: stage.Stage, Number: 1, LedgerTouching: ledger,
		}, eligible)
		if serr != nil {
			return nil, fmt.Errorf("workflow %q: %w", def.Name, serr)
		}
		pin := engine.PinnedPlacement{Stage: stage.Stage, LedgerTouching: ledger}
		if selected.HostKind == instance.RunnerHostSelf {
			pin.Self = true
		} else {
			pin.Queue = dispatcher.QueueName(gaggle, selected.Name)
			pin.Eligible = eligible
			pin.CPU = req.CPU
			pin.Memory = req.Memory
			pin.Disk = req.Disk
			pin.Restrictions = req.Restrictions
		}
		placements = append(placements, pin)
	}
	return placements, nil
}
