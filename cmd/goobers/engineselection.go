package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	wfpkg "github.com/goobers/goobers/internal/workflow"
)

// engineSelection is the per-entry answer to "does this lane dispatch onto the
// tier-3 engine, or stay on the local runner?".
type engineSelection struct {
	// UseEngine is the decision.
	UseEngine bool
	// FallbackReason names, in operator language, why the lane stayed on the
	// runner. Empty when UseEngine is true.
	FallbackReason string
	// SelfPinnedStages and UnpinnedGates are the two disqualifying sets,
	// carried separately so the per-tick annotation can name them as data
	// rather than only inside a rendered sentence.
	SelfPinnedStages []string
	UnpinnedGates    []string
	// Refusal is the engine walk's own refusal of this definition, when that
	// is what disqualified the lane rather than its placements. Kept as an
	// error so the annotation can classify with errors.Is.
	Refusal error
}

// selectEngineForEntry decides whether one scheduler entry dispatches through
// the engine.
//
// # The predicate, and why it is this one
//
// The obvious predicate — "engine is enabled, so use the engine" — is a
// four-lane production outage on any real instance, and the corrected
// predicate below is the whole reason D1 is a per-ENTRY decision rather than a
// daemon-wide switch.
//
// An engine-driven stage executes on a Temporal worker. A stage pinned to
// SELF is, by definition, a stage the placement solve says must execute on the
// daemon's own host — the worker cannot run it. And an agentic gate with no
// pin row at all still evaluates on the worker's self arm (engine.evaluateGate
// takes the remote arm only when remotePlacementFor finds a row), which is the
// same problem wearing a different hat. Either one, dispatched to the engine,
// produces a run that cannot make progress.
//
// Crucially, self-pinning is NOT an error state to be refused: every 2.0-DSL
// lane self-pins, because pre-3.0 StagePlacements emits task-only
// requiredCapabilities rows and no gate rows at all
// (internal/workflow/placement.go). A daemon that refused self-pinned lanes
// globally would stop dispatching them entirely. They must keep running,
// byte-for-byte, on the local runner — which is exactly what the fallback is.
//
// So an entry uses the engine only when ALL of:
//
//   - bootstrap.PinStagePlacements yielded a NON-EMPTY pin set. Nil is the
//     zero-declaration / local-mode instance (architecture §11 item 1's
//     invariance), where there is no remote runner to dispatch to at all.
//   - NO pin is Self.
//   - EVERY agentic gate in the definition has a pin row.
//   - the engine walk does not REFUSE the definition outright
//     (engine.RefuseDefinition): human gates and the R9 unsupported-feature
//     set contribute no placement rows, so without this check a fully pinned
//     lane declaring one would be selected and then refused on every tick,
//     having run perfectly well on the runner the day before.
//
// Per-lane rollback is therefore by DSL: reverting a lane's runsOn
// declarations moves it back to the runner on the next config reload, with no
// daemon flag and no restart.
func selectEngineForEntry(def wfpkg.Definition, placements []engine.PinnedPlacement) engineSelection {
	if len(placements) == 0 {
		return engineSelection{
			FallbackReason: "no stage placements are pinned for this workflow (zero-declaration or local-mode inventory)",
		}
	}
	pinned := make(map[string]engine.PinnedPlacement, len(placements))
	var selfPinned []string
	for _, p := range placements {
		pinned[p.Stage] = p
		if p.Self {
			selfPinned = append(selfPinned, p.Stage)
		}
	}
	var unpinnedGates []string
	for _, g := range def.Spec.Gates {
		if !agenticGate(g) {
			continue
		}
		if _, ok := pinned[g.Name]; !ok {
			unpinnedGates = append(unpinnedGates, g.Name)
		}
	}
	sort.Strings(selfPinned)
	sort.Strings(unpinnedGates)
	if len(selfPinned) > 0 || len(unpinnedGates) > 0 {
		return engineSelection{
			SelfPinnedStages: selfPinned,
			UnpinnedGates:    unpinnedGates,
			FallbackReason:   engineFallbackReason(selfPinned, unpinnedGates),
		}
	}
	// Asked LAST, so a lane that is disqualified on placement grounds still
	// reports the placement reason — the one an operator can act on with a
	// runsOn edit.
	if err := engine.RefuseDefinition(def.Name, def.Spec); err != nil {
		return engineSelection{
			Refusal:        err,
			FallbackReason: "the engine walk refuses this definition: " + err.Error(),
		}
	}
	return engineSelection{UseEngine: true}
}

// agenticGate reports whether a gate is evaluated by an agent — the class
// whose engine arm needs a remote placement. A deterministic gate evaluates
// in the workflow itself and needs no runner at all.
//
// Human gates are excluded here because a lane carrying one is disqualified
// outright by engine.RefuseDefinition (the walk refuses it at run start), so
// reporting it as "unpinned" would send an operator hunting for a runsOn
// declaration that would not help.
func agenticGate(g apiv1.Gate) bool {
	return g.Evaluator == apiv1.EvaluatorAgentic
}

// engineFallbackReason renders the operator-facing explanation, naming the
// stages and the plane client each of them needs.
//
// Naming the plane client is the part that makes the annotation actionable
// rather than merely informative: "stage X is self-pinned" tells an operator
// the lane did not move; "stage X is self-pinned (needs the dispatcher's
// remote runner plane)" tells them what has to exist before it can.
func engineFallbackReason(selfPinned, unpinnedGates []string) string {
	var parts []string
	if len(selfPinned) > 0 {
		parts = append(parts, fmt.Sprintf(
			"self-pinned stages [%s] need a non-self runner in the placement inventory (dispatcher remote-runner plane)",
			strings.Join(selfPinned, ", ")))
	}
	if len(unpinnedGates) > 0 {
		parts = append(parts, fmt.Sprintf(
			"agentic gates [%s] declare no runsOn, so they would evaluate on the worker's self arm; they need a runsOn declaration and the gate credential plane",
			strings.Join(unpinnedGates, ", ")))
	}
	return strings.Join(parts, "; ")
}

// engineSelections runs the per-entry engine/runner decision for every
// compiled workflow.
//
// It re-solves placements through bootstrap.PinStagePlacements — the SAME
// function the run's StartSpec is pinned from — rather than reusing
// placementRefusals' SolveExecutable pass. The two solves answer different
// questions (refusals asks "can this place at all?", this asks "where did
// each stage land?"), and only PinStagePlacements applies SelectRunner, which
// is what decides Self. Deciding selection from anything but the pins the run
// will actually carry would let a lane be routed to the engine on one
// function's opinion and dispatched with another's.
//
// A solve error is NOT a selection error: PinStagePlacements refuses an
// unplaceable stage, and that refusal is already surfaced per-run by
// PlacementRefusal (checkpoint 3). Here it means only "this lane does not go
// to the engine", with the diagnostic carried into the fallback annotation.
func engineSelections(
	cfg *instance.Config,
	set *instance.ConfigSet,
	machines map[localscheduler.WorkflowIdentity]*wfpkg.Machine,
) (map[localscheduler.WorkflowIdentity]engineSelection, error) {
	out := make(map[localscheduler.WorkflowIdentity]engineSelection, len(machines))
	// An instance with no engine configuration has nowhere to dispatch: every
	// lane stays on the runner, and the annotation says so rather than naming
	// placement facts that are not the reason.
	if cfg == nil || !cfg.EngineProjectionEnabled() {
		for identity := range machines {
			out[identity] = engineSelection{FallbackReason: "this instance has no engine configuration"}
		}
		return out, nil
	}
	for identity, machine := range machines {
		if machine == nil {
			continue
		}
		placements, err := bootstrap.PinStagePlacements(cfg, set, identity.Gaggle, machine.Def)
		if err != nil {
			out[identity] = engineSelection{
				FallbackReason: "stage placements could not be pinned: " + err.Error(),
			}
			continue
		}
		out[identity] = selectEngineForEntry(machine.Def, placements)
	}
	return out, nil
}

// entryStarterInput is everything selectEntryStarter needs to install one
// entry's Starter.
type entryStarterInput struct {
	runnerStarter localscheduler.Starter
	selection     engineSelection
	runtime       *engineRuntime
	hooks         *engineTerminalHooks

	gaggle string
	def    wfpkg.Definition
	spec   engineRunRequest

	layout     instance.Layout
	log        *journal.InstanceLog
	telemetry  *telemetry.Client
	rollupDB   *rollup.DB
	watermarks *intake.Store

	allowPreviewFeatures bool
	liveJournal          bool
	wg                   *sync.WaitGroup
}

// selectEntryStarter installs either the engine starter or the local runner's,
// wrapping the latter in the annotation that names why.
//
// The runner branch returns the ORIGINAL starter's behavior unchanged — the
// wrapper only appends one instance-log event before delegating — so a lane
// that stays on the runner runs byte for byte as it did before D1. That is
// the property per-lane rollback depends on: reverting a lane's runsOn
// declarations must restore its previous behavior exactly, not approximately.
func selectEntryStarter(in entryStarterInput) localscheduler.Starter {
	if !in.selection.UseEngine {
		return &runnerFallbackStarter{
			next:      in.runnerStarter,
			log:       in.log,
			workflow:  in.def.Name,
			selection: in.selection,
		}
	}
	return &engineStarter{
		runtime:              in.runtime,
		hooks:                in.hooks,
		gaggle:               in.gaggle,
		def:                  in.def,
		spec:                 in.spec,
		layout:               in.layout,
		log:                  in.log,
		telemetry:            in.telemetry,
		rollupDB:             in.rollupDB,
		watermarks:           in.watermarks,
		allowPreviewFeatures: in.allowPreviewFeatures,
		liveJournal:          in.liveJournal,
		wg:                   in.wg,
	}
}
