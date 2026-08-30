package main

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// engineRunRequest is everything one engine dispatch needs to build its
// StartSpec, whoever is dispatching: `goobers engine-start` on the command
// line, or the daemon's engineStarter on a cron/webhook/trigger-plane tick.
//
// It is a struct rather than a positional argument list because the inputs are
// otherwise a run of same-typed values — the gaggle name, the dedupe key, the
// trigger ref and the goober digest are all strings, and transposing any two
// compiles cleanly while pinning a different run identity. Naming them at the
// call site makes that transposition visible rather than silent.
//
// The workflow name is deliberately absent: it is def.Name. The caller
// resolved def for the requested name, so reading it back from the definition
// keeps the graph, the placements and the run controls sourced from one object
// instead of from a name that a second lookup might resolve elsewhere.
type engineRunRequest struct {
	cfg         *instance.Config
	set         *instance.ConfigSet
	gaggle      string
	project     apiv1.RepoRef
	def         workflow.Definition
	liveJournal bool

	// runID pins a run identity the caller already minted. The daemon's
	// scheduler mints its own run id when it admits a run (and records it in
	// admittedRuns, the slot ledger and the instance log) BEFORE the starter
	// is called, so the engine run must adopt that id or the instance log's
	// run.started and the run's own journal would name two different runs.
	// Empty derives the id from gaggle/workflow/dedupeKey, which is
	// engine-start's behavior.
	runID string
	// dedupeKey participates in the derived run id when runID is empty. It
	// is ignored when runID is set — see runEngineStart for why `--dedupe-key`
	// is refused outside `--direct`.
	dedupeKey string

	// triggerKind and triggerRef are the journal.Trigger vocabulary values
	// for what caused this run. The daemon passes the scheduler's decision
	// through verbatim so an engine-driven run's run.yaml trigger block is
	// the same one a runner-driven run would have recorded.
	triggerKind string
	triggerRef  string

	// item is the driving backlog item, when the trigger has one.
	item *apiv1.BacklogItem

	// gooberDigest is the kit digest the scheduler entry pins for this lane
	// (localscheduler.WorkflowEntry.GooberDigest). Provenance only — see
	// engine.StartSpec.GooberDigest and #3884.
	gooberDigest string
}

// engineRunSpec builds the StartSpec an engine dispatch pins.
//
// This is the ONLY place an engine.StartSpec is assembled. Before decision 005
// D1 there was one assembly site (engine-start's) and the daemon had none; the
// obvious way to give the daemon a starter would have been to write a second
// one next to the scheduler entry. That is exactly the failure the #3873 /
// #3820 / #294 regressions came from: each of BacklogQueryAssignedTo,
// RunControls and GateGooberCapabilities is silently zero-valued when a
// starter forgets it, and each zero value is a plausible-looking run that
// claims another instance's items, ignores the author's repass budget, or
// resolves every reviewer to no capabilities. One assembly site means a field
// added to StartSpec has exactly one place that must learn about it, and the
// two dispatch paths cannot drift.
func engineRunSpec(req engineRunRequest) (engine.StartSpec, error) {
	// Mode-3 placement pinning (#3588): resolve every stage's execution
	// placement now, against the declared runner inventory, and pin the
	// outcome into the run input — the workflow reads it as data and never
	// solves mid-run (the WF-016 snapshot / determinism constraint). Nil on
	// every zero-declaration and local-mode instance.
	placements, err := bootstrap.PinStagePlacements(req.cfg, req.set, req.gaggle, req.def)
	if err != nil {
		return engine.StartSpec{}, fmt.Errorf("resolve stage placements: %w", err)
	}
	// #3820: run controls are pinned at start and the watchdog enforces the
	// pinned value, so a starter that skips this resolution silently commits
	// the run to the 3-repass / 45m defaults whatever the author declared.
	controls, err := engineRunControls(req)
	if err != nil {
		return engine.StartSpec{}, err
	}
	runID := req.runID
	if runID == "" {
		runID = engine.RunID(req.gaggle, req.def.Name, req.dedupeKey)
	}
	triggerKind := req.triggerKind
	if triggerKind == "" {
		triggerKind = "manual"
	}
	triggerRef := req.triggerRef
	if triggerRef == "" {
		triggerRef = req.def.Name
	}
	return engine.StartSpec{
		RunID:           runID,
		Gaggle:          req.gaggle,
		RepoRef:         req.project,
		Item:            req.item,
		TriggerKind:     triggerKind,
		TriggerRef:      triggerRef,
		BranchNamespace: branchNamespacesByGaggle(req.set)[req.gaggle],
		LiveJournal:     req.liveJournal,
		Placements:      placements,
		RunControls:     controls,
		// #294/#3528: an agentic gate's reviewer capabilities are instance
		// policy, pinned into the run at start and read back from the run's
		// own snapshot afterwards — the daemon's credential plane resolves a
		// gate stage's grants from journal.PinnedGateGooberCapabilities, not
		// from the currently-served config. A starter that leaves this empty
		// pins an EMPTY map, so every gate branch on the run resolves to no
		// reviewer grants at all; the daemon's scheduler entry has always
		// filled it in (runnerwiring.go) and engine-start did not.
		GateGooberCapabilities: engineRunGateGooberCapabilities(req.set, req.gaggle),
		// #3873 (MIRC-2 claim partition): the gaggle's self identity and
		// RequireLabels default, resolved through the SAME helpers that feed
		// the daemon's runner Config (daemon.go). The engine walk injects
		// them into every `goobers backlog-query` stage that does not declare
		// its own, exactly as dispatchTask does. A dispatch that leaves them
		// empty hands the stage no partition, and on a shared backlog that
		// claims the sibling instance's goobers:local items.
		BacklogQueryAssignedTo:    selfIdentitiesByGaggle(req.cfg, req.set)[req.gaggle],
		BacklogQueryRequireLabels: requireLabelsByGaggle(req.set)[req.gaggle],
		// #3876: kit provenance, so an engine run's run.yaml names the same
		// digest gooberDigestStarter stamps on a runner-driven one.
		GooberDigest: req.gooberDigest,
	}, nil
}

// engineRunGateGooberCapabilities maps each goober a gaggle's stages may
// name to its declared capabilities. A goober with no declared capabilities is
// omitted rather than mapped to an empty slice; nil when the gaggle declares
// no capability-bearing goobers, which pins no snapshot at all rather than an
// empty one.
//
// The projection is GAGGLE-SCOPED, and deliberately so — it is NOT the
// daemon's map. runnerwiring.go's gateGooberCaps is built from goobersByName,
// which is instance-wide and shared across every gaggle; this one applies
// workerwiring.go's resolveGoobersForGaggle rule (a goober with no gaggle, or
// with this gaggle), because an engine run executes on a worker whose goober
// admission uses exactly that rule. Pinning a reviewer the worker will not
// admit would put a capability grant in the run's immutable snapshot for a
// goober that cannot participate in it.
//
// The divergence is fail-closed in one direction: a workflow in gaggle A whose
// agentic gate names a reviewer declared under gaggle B gets its grants pinned
// on the runner's own dispatch path and nothing pinned here, and the
// credential plane's gate branch resolves an absent reviewer to no grants.
// Nothing validates that cross-gaggle reference today; making it an error at
// compile time is the fix, not widening the pin.
func engineRunGateGooberCapabilities(set *instance.ConfigSet, gaggle string) map[string][]string {
	if set == nil {
		return nil
	}
	var caps map[string][]string
	for i := range set.Goobers {
		g := set.Goobers[i]
		if g.Spec.Gaggle != "" && g.Spec.Gaggle != gaggle {
			continue
		}
		if len(g.Spec.Capabilities) == 0 {
			continue
		}
		if caps == nil {
			caps = make(map[string][]string)
		}
		caps[g.Name] = append([]string(nil), g.Spec.Capabilities...)
	}
	return caps
}

// engineRunControls resolves the run-control policy for one engine dispatch
// through resolveWorkflowRunControls — the same four layers, in the same
// order, the daemon's scheduler entry resolves.
func engineRunControls(req engineRunRequest) (apiv1.RunControls, error) {
	// A request with no config set cannot resolve any layer. Refusing beats
	// defaulting for exactly the #3820 reason below, and beats panicking for
	// the obvious one.
	if req.set == nil {
		return apiv1.RunControls{}, fmt.Errorf(
			"engine run controls for %s/%s: no configuration set to resolve from", req.gaggle, req.def.Name)
	}
	var gaggleCfg apiv1.Gaggle
	for i := range req.set.Gaggles {
		if req.set.Gaggles[i].Name == req.gaggle {
			gaggleCfg = req.set.Gaggles[i]
			break
		}
	}
	// Resolution is layer-complete or it fails: silently defaulting a
	// workflow the config set does not carry is the #3820 failure mode.
	declared := false
	for i := range req.set.Workflows {
		if req.set.Workflows[i].Name == req.def.Name && req.set.Workflows[i].Spec.Gaggle == req.gaggle {
			declared = true
			break
		}
	}
	if !declared {
		return apiv1.RunControls{}, fmt.Errorf("workflow %q is not declared in gaggle %q", req.def.Name, req.gaggle)
	}
	// The workflow layer comes from the definition the caller pinned, NOT
	// from a fresh scan of set.Workflows. bootstrap.RegisterGaggleWorkflows
	// registers every entry in the gaggle, so two same-named workflow files
	// become v1 and v2 of one name; reg.Latest hands back the LAST, while a
	// forward scan finds the FIRST. Sourcing both from def is what keeps the
	// dispatched graph and its watchdog budget the same version.
	workflowCfg := apiv1.Workflow{Spec: req.def.Spec}
	controls, err := resolveWorkflowRunControls(req.cfg, req.project, gaggleCfg, workflowCfg)
	if err != nil {
		return apiv1.RunControls{}, fmt.Errorf("workflow %q run controls: %w", req.def.Name, err)
	}
	return controls.Overrides(), nil
}
