// Package runnersolve is the ONE shared constraint solver behind all three
// admission checkpoints (dsl-3.0.md §5, decision record D4; the shared home
// dsl-3.0.md open point 8 asks for): apply/validate's per-stage solve
// (checkpoint 1), the scheduler's per-run admit (checkpoint 2's self-runner
// half), and the boot pass that marks unsatisfiable workflows refused instead
// of killing the daemon (checkpoint 3, #2860). Checkpoints must consume this
// package rather than reimplementing any dimension of the match — the
// CAP003/scheduler mirror lesson: two implementations of "does this stage
// place?" diverge, and divergence produces configs that validate but never
// schedule (#3497).
//
// The solve is a pure function of declared inputs: the compiled stages'
// effective placement requirements (declared runsOn ∪ derived requirements ∪
// the gaggle floor — built by internal/workflow.StagePlacements) crossed with
// the resolved runner inventory (internal/instance's ResolvedRunners view,
// converted by Config.PlacementRunners). Per stage the output is the ELIGIBLE
// RUNNER SET, not just a boolean: the dispatch-time half of checkpoint 2 —
// Temporal queue derivation from the eligible set and the bounded
// Linux-preferring wait of decision D3 — is #3513's, and consumes these
// placements later, so eligibility must stay a deterministic function the
// dispatcher can re-derive. The Windows structural facts of
// goobernetes-architecture.md D12 (ledger-touching stages never place on
// Windows; higher Windows dispatch bounds) are likewise dispatch-time rules
// that land with #3513, not filters here.
//
// Matching dimensions (dsl-3.0.md §5 checkpoint 1):
//
//   - os: explicit-complete (D3) on both sides. A stage with no OS
//     requirement matches any runner; a stage requiring an OS matches only a
//     runner that claims exactly that OS. A runner claiming no OS satisfies
//     no OS requirement — claims are the only truth (D10).
//   - quantities: stage minimums against runner ceilings (≥ by Kubernetes
//     quantity comparison). An undeclared ceiling constrains nothing. On a
//     local-mode inventory (no non-self runner) quantities are ADVISORY —
//     warnings (RNR004), never lost eligibility (D4: "on local modes,
//     resource requirements are advisory, never errors").
//   - capabilities: exact set membership (internal/runnercap), stage requires
//     ⊆ runner provides. The self runner additionally satisfies the derived
//     namespace implicitly (see Runner.Self).
//   - restrictions: stage requires ⊆ runner enforces, with the instance
//     isolation mandate floor merged into every stage (Inventory.Mandates —
//     a seam today, see its comment).
package runnersolve

import (
	"fmt"
	"runtime"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/goobers/goobers/internal/runnercap"
)

// The three schedulable operating systems — the dsl-3.0.md D2 enum, product
// spellings ("macOS", not GOOS "darwin"). Mirrors apiv1's runsOn.os enum and
// internal/instance's provides.os enum by construction; all three quote
// dsl-3.0.md §2/§3 and the published schemas pin the workflow and instance
// sides with enum markers.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
	OSMacOS   = "macOS"
)

// HostOS maps the running process's GOOS onto the enum — the OS fact callers
// substitute for a self runner that declares none (the daemon host's OS is a
// process fact, not a trusted claim). An unschedulable GOOS returns "".
func HostOS() string {
	switch runtime.GOOS {
	case "linux":
		return OSLinux
	case "windows":
		return OSWindows
	case "darwin":
		return OSMacOS
	default:
		return ""
	}
}

// Runner is the solver's view of one declared runner (dsl-3.0.md §3):
// trusted claims only (decision record D10 — a false claim degrades to a
// runtime error, never a solver concern).
type Runner struct {
	// Name identifies the runner in diagnostics and placements.
	Name string
	// Self marks the daemon host itself (host: "self", or the implicit entry
	// a legacy singular runner: block resolves to). A self runner implicitly
	// satisfies every derived-namespace requirement (harness:<name>, shell —
	// internal/runnercap's derived vocabulary): the daemon host executes
	// agentic and shell stages through the local execution path with its
	// configured harness command, preflight-verified at startup (#238/#735).
	// Non-self runners must claim tokens exactly; the derived harness
	// spelling is deliberately not author-declarable (colon fails the
	// provides.capabilities token grammar), so how a runner image advertises
	// a harness is the dispatcher/image contract's to define (#3513, decision
	// record D8) — until then agentic stages are placeable only on self,
	// which is also the only host kind that executes anything today.
	// A self runner also implicitly enforces the network:none restriction:
	// the local executor's isolation mechanism ships today
	// (internal/executor/network_*.go) and dsl-3.0.md D16 keeps it as the
	// self-runner enforcement of the migrated run.network surface. The other
	// v1 effects have no local mechanism and must be declared.
	Self bool
	// OS is the runner's claimed operating system ("" claims none).
	OS string
	// CPU, Memory, and Disk are the runner's declared ceilings (provides
	// quantities — they become limits in mode 3). Nil means no declared
	// ceiling, which constrains nothing.
	CPU    *resource.Quantity
	Memory *resource.Quantity
	Disk   *resource.Quantity
	// Capabilities is the claim set, matched exactly (internal/runnercap).
	Capabilities []string
	// Restrictions are the isolation effects this runner enforces.
	Restrictions []string
}

// SelfRunner is the solver view of the daemon host for the scheduler's
// per-run admit (checkpoint 2's self-runner case): the configured claims,
// the host's own OS, no declared ceilings. This is what
// localscheduler.WithRunnerCapabilities builds, so the per-run check and the
// full solve read the self runner identically.
func SelfRunner(caps []string) Runner {
	return Runner{Name: "self", Self: true, OS: HostOS(), Capabilities: caps}
}

// MissingCapabilities returns the required capability tokens r does not
// satisfy, in first-appearance order and de-duplicated — byte-identical to
// runnercap.Claimed.Missing for every declared token, which is what keeps the
// scheduler's refusal diagnostics identical to previous releases for legacy
// configs (checkpoint 2's replacement contract). The one addition is the
// derived namespace: a self runner satisfies harness:<name> and shell
// implicitly (see Runner.Self); legacy required sets never contain harness
// tags (the author grammar cannot spell them), so the addition is invisible
// to 2.0 shapes.
func (r Runner) MissingCapabilities(required []string) []string {
	claimed := runnercap.NewClaimed(r.Capabilities)
	var missing []string
	seen := make(map[string]struct{}, len(required))
	for _, token := range required {
		if _, dup := seen[token]; dup {
			continue
		}
		seen[token] = struct{}{}
		if claimed.Has(token) {
			continue
		}
		if r.Self && runnercap.DerivedTag(token) {
			continue
		}
		missing = append(missing, token)
	}
	return missing
}

// enforces reports whether r enforces restriction effect.
func (r Runner) enforces(effect string) bool {
	if r.Self && effect == string(runnercap.RestrictionNetworkNone) {
		return true
	}
	for _, declared := range r.Restrictions {
		if declared == effect {
			return true
		}
	}
	return false
}

// StageRequirement is one stage's effective placement requirement: declared
// runsOn ∪ derived requirements ∪ the gaggle floor, as compiled by the
// workflow's own interpreter (internal/workflow.StagePlacements). For pre-3.0
// documents it degrades to the declared requiredCapabilities union — no OS,
// no quantities, no restrictions, no derivation (the frozen interpreters
// never learn the surface, PO-D0).
type StageRequirement struct {
	// Stage is the task name, for diagnostics.
	Stage string
	// OS is the required operating system ("" = no requirement, D3).
	OS string
	// CPU, Memory, and Disk are minimum quantities, verbatim Kubernetes
	// quantity strings ("" = no requirement). Malformed spellings are
	// compile/load errors upstream; the solver treats an unparsable minimum
	// as unsatisfiable by any declared ceiling rather than guessing.
	CPU    string
	Memory string
	Disk   string
	// Capabilities is the effective tag set (declared ∪ derived ∪ floor).
	Capabilities []string
	// Restrictions are the isolation effects the stage requires its runner
	// to enforce.
	Restrictions []string
}

// Inventory is the resolved runner inventory a solve runs against — pinned at
// daemon start (accept-and-pin, decision record D4/D9).
type Inventory struct {
	// Runners is the resolved inventory (declared runners:, or the implicit
	// self entry), in declaration order.
	Runners []Runner
	// Mandates is the instance isolation floor (decision record D7: "the
	// instance posture is a mandate"): effects merged into every stage's
	// restriction requirement before matching. SEAM — no instance.yaml
	// surface declares mandates yet (the restrictions companion doc owns
	// that shape); callers pass nil today, and when the config field lands
	// it plumbs through here without touching the match.
	Mandates []string
}

// LocalMode reports whether this inventory has no non-self runner — modes
// 1/2, where every stage executes on the daemon host and resource minimums
// are advisory (dsl-3.0.md D4). Mode is inferred from inventory shape
// (decision record D8).
func (inv Inventory) LocalMode() bool {
	for _, r := range inv.Runners {
		if !r.Self {
			return false
		}
	}
	return true
}

// UnsatKind classifies why a stage has an empty eligible set, which is what
// splits the RNR001 and RNR003 validation codes (dsl-3.0.md §5).
type UnsatKind string

const (
	// UnsatRequirement means no runner satisfies the stage's os /
	// capabilities / restrictions (RNR001).
	UnsatRequirement UnsatKind = "requirement"
	// UnsatQuantity means at least one runner satisfies the non-quantity
	// dimensions, but the stage's quantity minimums exceed every such
	// runner's declared ceiling (RNR003). Never produced in local mode,
	// where quantities are advisory.
	UnsatQuantity UnsatKind = "quantity"
)

// Unsat describes an unsatisfiable stage: its kind and a deterministic
// diagnostic naming the missing requirement per runner.
type Unsat struct {
	Kind UnsatKind
	// Diagnostic is the named, human-readable reason — stable wording, one
	// line, severity-neutral (callers wrap it in the RNR code and severity
	// their checkpoint assigns).
	Diagnostic string
}

// Advisory is a local-mode resource note (RNR004): the self runner's declared
// ceiling cannot cover a stage minimum. Advisory by design — never an error,
// never lost eligibility (dsl-3.0.md D4).
type Advisory struct {
	Stage string
	// Diagnostic names the resource, the minimum, and the ceiling.
	Diagnostic string
}

// StagePlacement is one stage's solve result.
type StagePlacement struct {
	Stage string
	// Eligible is the eligible runner set, in inventory order. Empty exactly
	// when Unsat is non-nil.
	Eligible []string
	// Unsat is set when no runner is eligible.
	Unsat *Unsat
	// Advisories are local-mode quantity notes (RNR004); a stage can be
	// eligible and still carry advisories.
	Advisories []Advisory
}

// Result is a full per-stage solve.
type Result struct {
	Stages []StagePlacement
}

// Unsatisfiable returns the unsatisfiable stages, in stage order.
func (r Result) Unsatisfiable() []StagePlacement {
	var out []StagePlacement
	for _, s := range r.Stages {
		if s.Unsat != nil {
			out = append(out, s)
		}
	}
	return out
}

// Solve crosses every stage requirement with the inventory. Deterministic:
// same inputs, same placements, same diagnostic strings.
func Solve(inv Inventory, stages []StageRequirement) Result {
	result := Result{Stages: make([]StagePlacement, 0, len(stages))}
	localMode := inv.LocalMode()
	for _, stage := range stages {
		result.Stages = append(result.Stages, solveStage(inv, stage, localMode))
	}
	return result
}

// runnerMatch is one runner's evaluation against one stage.
type runnerMatch struct {
	runner Runner
	// requirementMiss is the first os/capability/restriction mismatch (""
	// when the non-quantity dimensions are satisfied).
	requirementMiss string
	// quantityMisses name each minimum the runner's declared ceiling cannot
	// cover (empty when quantities are satisfied or undeclared).
	quantityMisses []string
}

func solveStage(inv Inventory, stage StageRequirement, localMode bool) StagePlacement {
	placement := StagePlacement{Stage: stage.Stage}
	restrictions := effectiveRestrictions(stage.Restrictions, inv.Mandates)
	matches := make([]runnerMatch, 0, len(inv.Runners))
	for _, runner := range inv.Runners {
		matches = append(matches, evaluateRunner(runner, stage, restrictions))
	}

	anyRequirementOK := false
	for _, m := range matches {
		if m.requirementMiss != "" {
			continue
		}
		anyRequirementOK = true
		if len(m.quantityMisses) == 0 {
			placement.Eligible = append(placement.Eligible, m.runner.Name)
			continue
		}
		if localMode {
			// Modes 1/2: quantities are advisory (RNR004) — the runner stays
			// eligible and the shortfall is named.
			placement.Eligible = append(placement.Eligible, m.runner.Name)
			placement.Advisories = append(placement.Advisories, Advisory{
				Stage: stage.Stage,
				Diagnostic: fmt.Sprintf("stage %q: the %q runner's declared ceiling cannot cover %s (resource minimums are advisory on a local-mode inventory)",
					stage.Stage, m.runner.Name, strings.Join(m.quantityMisses, ", ")),
			})
		}
	}
	if len(placement.Eligible) > 0 {
		return placement
	}
	if anyRequirementOK {
		placement.Unsat = &Unsat{Kind: UnsatQuantity, Diagnostic: quantityDiagnostic(stage, matches)}
	} else {
		placement.Unsat = &Unsat{Kind: UnsatRequirement, Diagnostic: requirementDiagnostic(stage, restrictions, matches)}
	}
	return placement
}

// effectiveRestrictions merges the instance mandate floor into a stage's own
// requirement, first-appearance order, de-duplicated.
func effectiveRestrictions(stage, mandates []string) []string {
	if len(mandates) == 0 {
		return stage
	}
	merged := make([]string, 0, len(stage)+len(mandates))
	seen := make(map[string]struct{}, len(stage)+len(mandates))
	for _, effect := range append(append([]string(nil), stage...), mandates...) {
		if _, dup := seen[effect]; dup {
			continue
		}
		seen[effect] = struct{}{}
		merged = append(merged, effect)
	}
	return merged
}

func evaluateRunner(runner Runner, stage StageRequirement, restrictions []string) runnerMatch {
	match := runnerMatch{runner: runner}
	switch {
	case stage.OS != "" && runner.OS == "":
		match.requirementMiss = fmt.Sprintf("claims no os (stage requires %q)", stage.OS)
	case stage.OS != "" && runner.OS != stage.OS:
		match.requirementMiss = fmt.Sprintf("provides os %q (stage requires %q)", runner.OS, stage.OS)
	}
	if match.requirementMiss == "" {
		if missing := runner.MissingCapabilities(stage.Capabilities); len(missing) > 0 {
			match.requirementMiss = fmt.Sprintf("is missing capabilities %s", strings.Join(missing, ", "))
		}
	}
	if match.requirementMiss == "" {
		var unenforced []string
		for _, effect := range restrictions {
			if !runner.enforces(effect) {
				unenforced = append(unenforced, effect)
			}
		}
		if len(unenforced) > 0 {
			match.requirementMiss = fmt.Sprintf("does not enforce restrictions %s", strings.Join(unenforced, ", "))
		}
	}
	for _, quantity := range []struct {
		name    string
		minimum string
		ceiling *resource.Quantity
	}{
		{name: "cpu", minimum: stage.CPU, ceiling: runner.CPU},
		{name: "memory", minimum: stage.Memory, ceiling: runner.Memory},
		{name: "disk", minimum: stage.Disk, ceiling: runner.Disk},
	} {
		if quantity.minimum == "" || quantity.ceiling == nil {
			// No requirement, or no declared ceiling to exceed.
			continue
		}
		minimum, err := resource.ParseQuantity(quantity.minimum)
		if err != nil {
			// Upstream validation rejects malformed quantities; fail closed
			// rather than silently matching if one reaches a solve anyway.
			match.quantityMisses = append(match.quantityMisses,
				fmt.Sprintf("%s minimum %q (unparsable) against ceiling %s", quantity.name, quantity.minimum, quantity.ceiling.String()))
			continue
		}
		if minimum.Cmp(*quantity.ceiling) > 0 {
			match.quantityMisses = append(match.quantityMisses,
				fmt.Sprintf("%s minimum %s (ceiling %s)", quantity.name, quantity.minimum, quantity.ceiling.String()))
		}
	}
	return match
}

// requirementDiagnostic renders the RNR001-class reason: what the stage
// requires and why each runner fails, in inventory order.
func requirementDiagnostic(stage StageRequirement, restrictions []string, matches []runnerMatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage %q requires %s; no runner satisfies it", stage.Stage, requirementSummary(stage, restrictions))
	if len(matches) > 0 {
		b.WriteString(": ")
		parts := make([]string, 0, len(matches))
		for _, m := range matches {
			miss := m.requirementMiss
			if miss == "" {
				// Only reachable when the stage is quantity-unsatisfiable and
				// this helper is used for context; keep the clause truthful.
				miss = "satisfies the requirement"
			}
			parts = append(parts, fmt.Sprintf("runner %q %s", m.runner.Name, miss))
		}
		b.WriteString(strings.Join(parts, "; "))
	}
	return b.String()
}

// quantityDiagnostic renders the RNR003-class reason: the stage's minimums
// exceed every otherwise-eligible runner's declared ceiling.
func quantityDiagnostic(stage StageRequirement, matches []runnerMatch) string {
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		if m.requirementMiss != "" || len(m.quantityMisses) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("runner %q ceiling cannot cover %s", m.runner.Name, strings.Join(m.quantityMisses, ", ")))
	}
	return fmt.Sprintf("stage %q resource minimums exceed every eligible runner's declared ceiling: %s",
		stage.Stage, strings.Join(parts, "; "))
}

// requirementSummary renders what the stage asks for, for diagnostics.
func requirementSummary(stage StageRequirement, restrictions []string) string {
	var parts []string
	if stage.OS != "" {
		parts = append(parts, fmt.Sprintf("os %q", stage.OS))
	}
	if len(stage.Capabilities) > 0 {
		parts = append(parts, fmt.Sprintf("capabilities [%s]", strings.Join(stage.Capabilities, ", ")))
	}
	if len(restrictions) > 0 {
		parts = append(parts, fmt.Sprintf("restrictions [%s]", strings.Join(restrictions, ", ")))
	}
	if len(parts) == 0 {
		return "only the base runner contract"
	}
	return strings.Join(parts, ", ")
}
