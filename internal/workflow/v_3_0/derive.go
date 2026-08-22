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

// DerivedCapabilities returns the placement requirements task carries by
// construction (D7), independent of any declared runsOn. goobers supplies the
// referenced goober specs for harness derivation; with a nil map an agentic
// stage derives the schema-default harness (copilot), matching admission's
// default resolution.
func DerivedCapabilities(task apiv1.Task, goobers map[string]apiv1.GooberSpec) []string {
	switch task.Type {
	case apiv1.TaskAgentic:
		harness := apiv1.HarnessCopilot
		if goober, ok := goobers[task.Goober]; ok && goober.Harness != "" {
			harness = goober.Harness
		}
		return []string{DerivedHarnessTagPrefix + string(harness)}
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

// EffectiveCapabilities is a stage's full placement requirement tag set:
// derived ∪ declared ∪ the gaggle floor (dsl-3.0.md §2 merge rule), sorted
// and de-duplicated. Declaration never subtracts — union only. OS-unspecified
// stages contribute no OS here or anywhere: unspecified means no requirement
// (D3); placement preference is policy the solver owns, never a compiled-in
// requirement.
func EffectiveCapabilities(task apiv1.Task, gaggleRunsOn *apiv1.GaggleRunsOn, goobers map[string]apiv1.GooberSpec) []string {
	tags := DerivedCapabilities(task, goobers)
	if task.RunsOn != nil {
		tags = append(tags, task.RunsOn.Capabilities...)
	}
	if gaggleRunsOn != nil {
		tags = append(tags, gaggleRunsOn.Capabilities...)
	}
	return sortedDistinct(tags)
}
