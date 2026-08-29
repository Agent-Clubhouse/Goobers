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

// PinStagePlacements resolves each placeable stage's execution placement —
// every task, and every agentic gate that declares runsOn (decision 001) —
// for one engine-started run of def and returns the pinned list for
// engine.StartSpec.Placements.
//
// Pins are keyed by STAGE NAME: each solver row is looked up by
// StageRequirement.Stage against the task and gate lists, never by position
// into def.Spec.Tasks. The index coupling this replaced was the one place a
// wrong refactor could silently attach a task's ledger/CPU facts to a gate
// once gate rows followed task rows (decision 001 ruling 6). A gate is never
// ledger-touching: only a task's PolicyActions can name a claims action.
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

	// Name keying is only as safe as name uniqueness: a task and a gate that
	// share a name would let the second insert silently overwrite the
	// first's facts — the inverse of the mis-attribution ruling 6 exists to
	// prevent. The 3.0 compiler already refuses duplicate state names
	// (structuralProblems), so this is defence in depth for a definition
	// that reaches here uncompiled: never a silent overwrite.
	requirementFor := make(map[string]runnersolve.StageRequirement, len(requirements))
	for _, requirement := range requirements {
		if _, dup := requirementFor[requirement.Stage]; dup {
			return nil, fmt.Errorf("workflow %q: placement requirements name stage %q twice; stage names must be unique for name-keyed pinning", def.Name, requirement.Stage)
		}
		requirementFor[requirement.Stage] = requirement
	}
	ledgerFor := make(map[string]bool, len(def.Spec.Tasks)+len(def.Spec.Gates))
	isGate := make(map[string]bool, len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		if _, dup := ledgerFor[task.Name]; dup {
			return nil, fmt.Errorf("workflow %q: task name %q is declared twice; stage names must be unique for name-keyed pinning", def.Name, task.Name)
		}
		ledgerFor[task.Name] = taskLedgerTouching(task)
	}
	for _, gate := range def.Spec.Gates {
		if _, dup := ledgerFor[gate.Name]; dup {
			return nil, fmt.Errorf("workflow %q: stage name %q is declared as both a task and a gate (or twice as a gate); stage names must be unique for name-keyed pinning", def.Name, gate.Name)
		}
		ledgerFor[gate.Name] = false
		isGate[gate.Name] = true
	}

	result := runnersolve.Solve(inventory, requirements)
	placements := make([]engine.PinnedPlacement, 0, len(result.Stages))
	for _, stage := range result.Stages {
		if stage.Unsat != nil {
			return nil, fmt.Errorf("workflow %q stage %q cannot place on the declared runners: inventory: %s", def.Name, stage.Stage, stage.Unsat.Diagnostic)
		}
		req, ok := requirementFor[stage.Stage]
		if !ok {
			return nil, fmt.Errorf("workflow %q: solver returned stage %q which the workflow's placement requirements do not name", def.Name, stage.Stage)
		}
		ledger, ok := ledgerFor[stage.Stage]
		if !ok {
			return nil, fmt.Errorf("workflow %q: placement requirement names stage %q which is neither a task nor a gate", def.Name, stage.Stage)
		}
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
		} else if isGate[stage.Stage] {
			// HOLD until decision 001's engine half (rulings 7–8) lands:
			// engine.evaluateGate has no placement arm — an agentic gate
			// always runs ActReviewGoober in-process on the workflow queue,
			// so a remote gate pin would be manufactured and then ignored,
			// and the reviewer would run with the worker's OS, network and
			// envelope instead of the isolation the author declared. That
			// is the insecure half; refuse the START with the placement
			// named instead. Mirrors checkpoint 3's posture for
			// daemon-scheduled runs (placementRefusals solves gates against
			// the self-only substrate). REMOVE this branch, WF024 and their
			// tests together when evaluateGate honours a non-self gate pin.
			return nil, fmt.Errorf(
				"workflow %q gate %q: its runsOn places the reviewer on runner %q (queue %s), but gate placement is not honoured at execution yet (decision 001 rulings 7–8, the engine/pod half, are unlanded) — the reviewer would evaluate in the control plane outside its declared isolation; refusing the start. Declare a gate placement self satisfies, or remove runsOn from the gate, until the engine half lands",
				def.Name, stage.Stage, selected.Name, dispatcher.QueueName(gaggle, selected.Name))
		} else {
			pin.Queue = dispatcher.QueueName(gaggle, selected.Name)
			pin.Eligible = eligible
			pin.CPU = req.CPU
			pin.Memory = req.Memory
			pin.Disk = req.Disk
			pin.Restrictions = req.Restrictions
			pin.Capabilities = req.Capabilities
		}
		placements = append(placements, pin)
	}
	return placements, nil
}
