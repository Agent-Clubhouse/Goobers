package v30

// derive.go computes DERIVED placement requirements (dsl-3.0.md §2 "Base
// contract and derivation", decisions D6/D7): the requirements a stage has by
// construction, which merge with declared runsOn.capabilities by union — a
// declaration can add to the derived set but never subtract from it. Tier-1/2
// authors write no runsOn at all and lose nothing.
//
//   - The BASE RUNNER CONTRACT (goobers binary, stage-contract environment,
//     network to provider endpoints, credential delivery) is implicit on every
//     runner and deliberately NOT spellable as a tag (D6) — it never appears
//     in a derived set.
//   - Agentic stages derive "harness:<name>" from the goober's existing
//     harness: field — declared once, never re-typed per stage; this is what
//     lets harness-less runner images exist (PO-D8's minimal goobers-base).
//     An agentic GATE derives the same tag from its reviewer goober
//     (decision 001: placement is a stage property, so a placed gate and a
//     placed task read one derivation rule).
//   - sh/make stages — any deterministic stage that shells out to something
//     other than the goobers binary, or runs an inline script — derive
//     "run:shell".
//   - Builtin stages (goobers <subcommand>) derive their needs from stage
//     identity via the provider-stage manifest resolved at this interpreter's
//     DSL version (providerstage.ForVersion, the D7 prerequisite that landed
//     as #3522). The manifest carries no runner-needs columns yet — every
//     builtin today needs only the base contract — so builtins derive the
//     empty set; when a builtin first needs a non-base placement, the column
//     is added to the manifest 3.0-gated (sinceDSL) so the frozen 2.0
//     interpreter can never see it. Recording that as data now would be a
//     dead, all-empty column.
//
// Derived tags use the colon-namespaced "harness:<name>"/"run:shell"
// spellings. These are system-derived facts, not author-declared tokens, so
// every one of them FAILS the runnercap author-token grammar by design (the
// colon is rejected) — an author-spelled plain "shell" is an ordinary
// capability token, never a derived tag; the spellings are owned by
// internal/runnercap (the leaf both the deriving and the matching side
// import), and how each runner kind satisfies them is the shared solver's
// contract (internal/runnersolve, #3506).

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/runnercap"
)

// DerivedShellTag is the derived requirement of a stage that shells out.
const DerivedShellTag = runnercap.DerivedShellTag

// DerivedHarnessTagPrefix prefixes the derived requirement of an agentic
// stage: DerivedHarnessTagPrefix + the goober's harness name.
const DerivedHarnessTagPrefix = runnercap.DerivedHarnessTagPrefix

// PlacementStage is the placement-relevant view of one stage — a task or an
// agentic gate. Decision 001 makes placement a STAGE property: tasks and
// gates share one runsOn block, one gaggle-floor merge (EffectiveRunsOn),
// and one derivation rule (EffectiveCapabilities), so the solver input and
// every runsOn check read this view rather than the task or gate type.
type PlacementStage struct {
	// Kind labels diagnostics: "task" or "gate".
	Kind string
	// Name is the stage name — the key every checkpoint and the run-start
	// pin (bootstrap.PinStagePlacements) look placements up by.
	Name string
	// RunsOn is the stage's declared block; nil declares nothing.
	RunsOn *apiv1.RunsOn
	// Derived is the D7 derived requirement tag set the stage carries by
	// construction, independent of RunsOn.
	Derived []string
}

const (
	stageKindTask = "task"
	stageKindGate = "gate"
)

func taskPlacementStage(task apiv1.Task, goobers map[string]apiv1.GooberSpec) PlacementStage {
	return PlacementStage{Kind: stageKindTask, Name: task.Name, RunsOn: task.RunsOn, Derived: DerivedCapabilities(task, goobers)}
}

func gatePlacementStage(gate apiv1.Gate, goobers map[string]apiv1.GooberSpec) PlacementStage {
	return PlacementStage{Kind: stageKindGate, Name: gate.Name, RunsOn: gate.RunsOn, Derived: DerivedGateCapabilities(gate, goobers)}
}

// harnessFor resolves the harness an agentic stage's goober runs on; with a
// nil map, or a goober declaring no harness, it is the schema-default
// harness (copilot), matching admission's default resolution.
func harnessFor(goober string, goobers map[string]apiv1.GooberSpec) apiv1.Harness {
	if spec, ok := goobers[goober]; ok && spec.Harness != "" {
		return spec.Harness
	}
	return apiv1.HarnessCopilot
}

// DerivedCapabilities returns the placement requirements task carries by
// construction (D7), independent of any declared runsOn. goobers supplies the
// referenced goober specs for harness derivation; with a nil map an agentic
// stage derives the schema-default harness (copilot), matching admission's
// default resolution.
func DerivedCapabilities(task apiv1.Task, goobers map[string]apiv1.GooberSpec) []string {
	switch task.Type {
	case apiv1.TaskAgentic:
		return []string{DerivedHarnessTagPrefix + string(harnessFor(task.Goober, goobers))}
	case apiv1.TaskDeterministic:
		if task.Run == nil {
			return nil
		}
		if len(task.Run.Command) >= 1 && task.Run.Command[0] == "goobers" {
			// Builtin: needs derive from stage identity via the versioned
			// manifest view; today that view carries no runner-needs beyond
			// the implicit base contract (see the file comment).
			return nil
		}
		if isShellStage(task) {
			return []string{DerivedShellTag}
		}
		return nil
	default:
		return nil
	}
}

// DerivedGateCapabilities returns the placement requirements gate carries by
// construction (D7 applied to gates, decision 001): an agentic gate derives
// harness:<name> from its REVIEWER goober's harness, by the identical rule an
// agentic task uses. Automated and human gates evaluate in the control plane
// and derive nothing.
func DerivedGateCapabilities(gate apiv1.Gate, goobers map[string]apiv1.GooberSpec) []string {
	if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil {
		return nil
	}
	return []string{DerivedHarnessTagPrefix + string(harnessFor(gate.Agentic.Goober, goobers))}
}

// EffectiveCapabilities is a stage's full placement requirement tag set:
// derived ∪ declared ∪ the gaggle floor (dsl-3.0.md §2 merge rule), sorted
// and de-duplicated. Declaration never subtracts — union only. OS-unspecified
// stages contribute no OS here or anywhere: unspecified means no requirement
// (D3); placement preference is policy the solver owns, never a compiled-in
// requirement.
func EffectiveCapabilities(stage PlacementStage, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	tags := append([]string(nil), stage.Derived...)
	if stage.RunsOn != nil {
		tags = append(tags, stage.RunsOn.Capabilities...)
	}
	if gaggleRunsOn != nil {
		tags = append(tags, gaggleRunsOn.Capabilities...)
	}
	return sortedDistinct(tags)
}
