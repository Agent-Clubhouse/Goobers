package v30

// runson.go validates the DSL 3.0 scheduling surface (dsl-3.0.md §2, decision
// record D2): the stage-level runsOn block, the gaggle-level floor it merges
// with, the CAP004 os=* token ban, the CAP005 closed restriction vocabulary,
// and the refusal of surface that no longer exists in 3.0.
//
// Quantity handling (dsl-3.0.md open point 6, decided here): quantities are
// parsed with the vendored k8s.io/apimachinery resource parser — the exact
// grammar the pod spec will consume in mode 3 and the same parser the
// instance inventory already uses (internal/instance/runners.go), so the two
// sides of the constraint solve can never disagree about well-formedness.
// Diagnostics and digests keep the author's verbatim spelling: D4 pins
// "Kubernetes quantity strings verbatim", and the definition digest hashes
// the raw document, so no canonicalization (2000m vs 2) is ever applied.

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
)

// runsOnOSValues is the validated os enum (D2): the product spellings, not
// GOOS (`macOS`, not `darwin`). It mirrors the instance inventory's
// provides.os enum (internal/instance RunnerOS) by construction — both quote
// dsl-3.0.md §2 — and the shared schema pins both with enum markers.
var runsOnOSValues = []string{"linux", "windows", "macOS"}

func validRunsOnOS(os string) bool {
	for _, v := range runsOnOSValues {
		if os == v {
			return true
		}
	}
	return false
}

// removedSurfaceProblems refuses 2.0 surface that does not exist in DSL 3.0,
// each with a pointer to its 3.0 home so the error is actionable without
// reading the design doc:
//
//   - task/gaggle requiredCapabilities → runsOn.capabilities (D1/D12); the
//     gaggle-level check lives in runsOnProblems because it needs the gaggle.
//   - run.network: none → runsOn.restrictions: [network:none] (D16 — one
//     restrictions model, not two network surfaces).
//
// The gaggle sandbox override (featureGaggleSandbox) is likewise gone in 3.0;
// FeaturesForGaggle refuses it with the same pointer-diagnostic style.
func removedSurfaceProblems(def Definition) []string {
	var problems []string
	for _, task := range def.Spec.Tasks {
		if task.RequiredCapabilities != nil {
			problems = append(problems, fmt.Sprintf(
				"task %q declares requiredCapabilities, which does not exist in DSL 3.0: move the tokens to runsOn.capabilities and any os=* token to runsOn.os (`goobers fix --to 3.0` performs this rewrite)",
				task.Name))
		}
		if task.Run != nil && task.Run.Network != "" {
			problems = append(problems, fmt.Sprintf(
				"task %q declares run.network, which does not exist in DSL 3.0: declare runsOn.restrictions: [%s] instead (one restrictions model, dsl-3.0.md D16)",
				task.Name, runnercap.RestrictionNetworkNone))
		}
	}
	return problems
}

// runsOnStages lists every stage whose declared runsOn block the structural
// and vocabulary checks read: every task (declared or not — the checks skip a
// nil block themselves), then every gate that declares one, evaluator
// notwithstanding. A non-agentic gate's block is refused separately by
// gateRunsOnProblems (WF023); its spelling is still checked here so one
// compile reports every problem. Derivation is not needed by these checks,
// so Derived stays nil.
func runsOnStages(def Definition) []PlacementStage {
	stages := make([]PlacementStage, 0, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		stages = append(stages, PlacementStage{Kind: stageKindTask, Name: task.Name, RunsOn: task.RunsOn})
	}
	for _, gate := range def.Spec.Gates {
		if gate.RunsOn == nil {
			continue
		}
		stages = append(stages, PlacementStage{Kind: stageKindGate, Name: gate.Name, RunsOn: gate.RunsOn})
	}
	return stages
}

// gateRunsOnProblems is WF023 (decision 001, dsl-3.0.md §2 "Gates"): the two
// gate-only rules on a declared gates[].runsOn block.
//
//   - Ruling 2: only an AGENTIC gate is placeable. An automated gate is a pure
//     function over its inputs and a human gate pauses for a portal decision;
//     both evaluate in the daemon/control plane by definition, so runsOn on
//     either is a compile error, never a silently ignored block.
//   - Ruling 5: a placed gate must carry quantities. The gaggle floor
//     deliberately has no cpu/memory, and an agentic review is the most
//     expensive stage class in a lane; inheriting the floor's capabilities
//     with no envelope would be a silent under-provision. So a gate runsOn
//     without explicit cpu AND memory is a compile error, not a default —
//     default-to-self would make "did my gate place?" invisible in the yaml.
//   - A placed gate must name its reviewer. The reviewer's harness is the
//     gate's derived requirement (DerivedGateCapabilities reads
//     gate.Agentic.Goober); an agentic gate with runsOn but no agentic:
//     block would otherwise solve with NO harness tag and could place on a
//     harness-less runner image. api/validate's GT-016 cardinality check
//     (WF014) catches the shape on the config-tree path; this is the
//     interpreter's own fail-closed arm so the API path and the CRD cannot
//     reach placeableStages with it.
func gateRunsOnProblems(def Definition) []string {
	var problems []string
	for _, gate := range def.Spec.Gates {
		if gate.RunsOn == nil {
			continue
		}
		if gate.Evaluator != apiv1.EvaluatorAgentic {
			problems = append(problems, fmt.Sprintf(
				"gate %q declares runsOn but its evaluator is %q: only an agentic gate is placeable — automated and human gates evaluate in the daemon/control plane by definition (decision 001, dsl-3.0.md §2); remove runsOn from the gate",
				gate.Name, gate.Evaluator))
			continue
		}
		if gate.Agentic == nil {
			problems = append(problems, fmt.Sprintf(
				"gate %q declares runsOn but has no agentic: block naming its reviewer goober, so the reviewer's harness requirement cannot be derived and the gate could place on a runner without one (decision 001, dsl-3.0.md §2); add agentic.goober or remove runsOn",
				gate.Name))
			continue
		}
		var missing []string
		if gate.RunsOn.CPU == "" {
			missing = append(missing, "cpu")
		}
		if gate.RunsOn.Memory == "" {
			missing = append(missing, "memory")
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf(
				"gate %q declares runsOn without %s: a placed agentic gate must declare cpu and memory explicitly — the gaggle floor carries no quantities and a review is the most expensive stage class in a lane, so an inherited envelope would silently under-provision the reviewer (decision 001 ruling 5, dsl-3.0.md §2)",
				gate.Name, strings.Join(missing, " and ")))
		}
	}
	return problems
}

// gatePlacementWarnings is WF024: one warning per agentic gate that declares
// runsOn, for as long as decision 001's engine/pod half (rulings 7–8:
// evaluateGate through the dispatch seam, a review mode on the agentic kit,
// the surrendered verdict) is unlanded. Today the block is compiled, solved
// (RNR001/RNR003) and pinned by name, but engine.evaluateGate has no
// placement arm: an agentic gate always runs ActReviewGoober in-process on
// the workflow's own queue. Accepting a declared isolation set and running
// the reviewer outside it silently would be the insecure half, so the two
// start seams fail closed — a placement self cannot satisfy is refused
// (checkpoint 3 for daemon-scheduled runs; bootstrap.PinStagePlacements for
// engine-start) — and this warning tells the author so at validate time.
//
// REMOVE with the engine half: once evaluateGate honours a non-self gate
// pin, this function and its WF024 code retire together with the
// PinStagePlacements refusal.
func gatePlacementWarnings(def Definition) []string {
	var warnings []string
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.RunsOn == nil {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"gate %q declares runsOn: the block is validated, solved and pinned by name, but no execution path honours a gate placement yet (decision 001 rulings 7–8, the engine/pod half, land separately) — the reviewer still evaluates in the daemon/control plane with that host's OS, network and envelope. A placement self satisfies pins self and evaluates in-process; one self cannot satisfy is refused at start (workflow.refused for daemon-scheduled runs, a named error for engine-start) rather than run outside its declared isolation",
			gate.Name))
	}
	return warnings
}

// runsOnProblems reports structural problems in the declared runsOn blocks
// (tasks and gates alike — runsOnStages): an os value outside the validated
// enum, a malformed or non-positive quantity, a malformed capability token,
// and a gaggle-vs-stage OS conflict (the merge rule of dsl-3.0.md §2:
// capabilities and restrictions union; an OS conflict is a compile error,
// never a silent override).
func runsOnProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	gaggleOS := ""
	if gaggleRunsOn != nil {
		gaggleOS = gaggleRunsOn.OS
		if gaggleOS != "" && !validRunsOnOS(gaggleOS) {
			problems = append(problems, fmt.Sprintf(
				"gaggle runsOn.os %q is not one of %s", gaggleOS, strings.Join(runsOnOSValues, ", ")))
		}
		for i, token := range gaggleRunsOn.Capabilities {
			if err := runnercap.ValidateToken(token); err != nil {
				problems = append(problems, fmt.Sprintf("gaggle runsOn.capabilities[%d]: %v", i, err))
			}
		}
	}
	for _, stage := range runsOnStages(def) {
		runsOn := stage.RunsOn
		if runsOn == nil {
			continue
		}
		if runsOn.OS != "" && !validRunsOnOS(runsOn.OS) {
			problems = append(problems, fmt.Sprintf(
				"%s %q runsOn.os %q is not one of %s", stage.Kind, stage.Name, runsOn.OS, strings.Join(runsOnOSValues, ", ")))
		}
		if runsOn.OS != "" && gaggleOS != "" && runsOn.OS != gaggleOS && validRunsOnOS(runsOn.OS) && validRunsOnOS(gaggleOS) {
			problems = append(problems, fmt.Sprintf(
				"%s %q runsOn.os %q conflicts with the gaggle-level runsOn.os %q; the gaggle floor merges into every stage and an OS conflict is unsatisfiable (dsl-3.0.md §2)",
				stage.Kind, stage.Name, runsOn.OS, gaggleOS))
		}
		for _, quantity := range []struct {
			field string
			value string
		}{
			{field: "cpu", value: runsOn.CPU},
			{field: "memory", value: runsOn.Memory},
			{field: "disk", value: runsOn.Disk},
		} {
			if quantity.value == "" {
				continue
			}
			parsed, err := resource.ParseQuantity(quantity.value)
			if err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s %q runsOn.%s %q must be a Kubernetes quantity string (for example \"2000m\", \"4Gi\"): %v",
					stage.Kind, stage.Name, quantity.field, quantity.value, err))
				continue
			}
			if parsed.Sign() <= 0 {
				problems = append(problems, fmt.Sprintf(
					"%s %q runsOn.%s must be positive, got %q", stage.Kind, stage.Name, quantity.field, quantity.value))
			}
		}
		for i, token := range runsOn.Capabilities {
			if err := runnercap.ValidateToken(token); err != nil {
				problems = append(problems, fmt.Sprintf("%s %q runsOn.capabilities[%d]: %v", stage.Kind, stage.Name, i, err))
			}
		}
	}
	return append(problems, windowsAdminProblems(def, gaggleRunsOn)...)
}

// windowsAdminProblems is the stage-side coherence rule for the one
// product-interpreted capability token (#3619, runnercap.CapabilityWindowsAdmin):
// a stage whose EFFECTIVE requirement (declared ∪ gaggle floor) carries
// privilege=windows-admin must have an effective runsOn.os of windows —
// declared explicitly, on the stage or by the floor. The token names a
// Windows container identity (ContainerAdministrator); on any other OS it is
// meaningless, and on an unset OS it would place only by the accident of
// which runners happen to claim it. Explicit-complete (D3) cuts the other
// way here: an admin-requiring stage is exactly the stage whose OS must be
// visible in the yaml. A floor that carries the token under a non-Windows
// gaggle OS is reported once at the gaggle, not once per stage.
func windowsAdminProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	if gaggleRunsOn != nil && runnercap.HasWindowsAdmin(gaggleRunsOn.Capabilities) &&
		gaggleRunsOn.OS != "" && gaggleRunsOn.OS != runnersolve.OSWindows {
		return []string{fmt.Sprintf(
			"gaggle runsOn.capabilities requires %q, the ContainerAdministrator identity of a Windows stage pod, but gaggle runsOn.os is %q: the privilege exists only on runsOn.os: windows (#3619)",
			runnercap.CapabilityWindowsAdmin, gaggleRunsOn.OS)}
	}
	for _, stage := range runsOnStages(def) {
		effective := EffectiveRunsOn(stage, gaggleRunsOn)
		if !runnercap.HasWindowsAdmin(effective.Capabilities) || effective.OS == runnersolve.OSWindows {
			continue
		}
		have := "unset"
		if effective.OS != "" {
			have = fmt.Sprintf("%q", effective.OS)
		}
		problems = append(problems, fmt.Sprintf(
			"%s %q runsOn.capabilities requires %q, the ContainerAdministrator identity of a Windows stage pod, but its effective runsOn.os is %s: declare runsOn.os: windows explicitly (on the stage or the gaggle floor) — the privilege is refused everywhere else, never defaulted (#3619)",
			stage.Kind, stage.Name, runnercap.CapabilityWindowsAdmin, have))
	}
	return problems
}

// osTokenProblems is CAP004 (dsl-3.0.md D12): an os=* token anywhere in a 3.0
// document is refused — runsOn.os is the only platform vocabulary, so the
// #659 two-vocabularies drift hazard structurally cannot recur. It scans
// every capability tag position: task and gate runsOn.capabilities and the
// gaggle-level floor. (requiredCapabilities cannot carry one because the
// whole field is already refused by removedSurfaceProblems; that refusal
// names the os=* rewrite too.)
func osTokenProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	if gaggleRunsOn != nil {
		for _, token := range gaggleRunsOn.Capabilities {
			if goos, ok := strings.CutPrefix(token, "os="); ok {
				problems = append(problems, fmt.Sprintf(
					"gaggle runsOn.capabilities contains %q: os=* tokens do not exist in DSL 3.0 — declare runsOn.os: %s instead", token, canonicalOSName(goos)))
			}
		}
	}
	for _, stage := range runsOnStages(def) {
		if stage.RunsOn == nil {
			continue
		}
		for _, token := range stage.RunsOn.Capabilities {
			if goos, ok := strings.CutPrefix(token, "os="); ok {
				problems = append(problems, fmt.Sprintf(
					"%s %q runsOn.capabilities contains %q: os=* tokens do not exist in DSL 3.0 — declare runsOn.os: %s instead", stage.Kind, stage.Name, token, canonicalOSName(goos)))
			}
		}
	}
	return problems
}

// canonicalOSName maps a GOOS spelling from a legacy os=* token to the 3.0
// enum spelling for the CAP004 rewrite hint (darwin → macOS, D12).
func canonicalOSName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux", "windows":
		return goos
	default:
		return fmt.Sprintf("<one of %s>", strings.Join(runsOnOSValues, "|"))
	}
}

// restrictionProblems is CAP005 (dsl-3.0.md §5): every restriction token must
// come from the closed v1 effect list (internal/runnercap, the vocabulary the
// instance inventory shares); an unknown token errors with a did-you-mean
// suggestion (the CAP002 idiom).
func restrictionProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	if gaggleRunsOn != nil {
		for _, token := range gaggleRunsOn.Restrictions {
			if !runnercap.KnownRestriction(token) {
				problems = append(problems, fmt.Sprintf("gaggle runsOn.restrictions: %s", unknownRestriction(token)))
			}
		}
	}
	for _, stage := range runsOnStages(def) {
		if stage.RunsOn == nil {
			continue
		}
		for _, token := range stage.RunsOn.Restrictions {
			if !runnercap.KnownRestriction(token) {
				problems = append(problems, fmt.Sprintf("%s %q runsOn.restrictions: %s", stage.Kind, stage.Name, unknownRestriction(token)))
			}
		}
	}
	return append(problems, windowsRestrictionProblems(def, gaggleRunsOn)...)
}

// windowsRestrictionProblems is the OS-conditional half of CAP005
// (restrictions doc D4 as corrected by #3619; acceptance criterion 2): a
// stage whose EFFECTIVE placement is windows (runsOn.os declared on the
// stage or inherited from the gaggle floor) may require only the effects
// Windows can bind — runnercap.DeclarableOnWindows: tmp:ephemeral and
// env:default-deny. The closed list was OS-blind here before: a Windows task
// could require network:none or fs:readonly-except-workspace, validate
// clean, and then either fail to place (the honest outcome, but late and
// named by the solver rather than the vocabulary) or — worse, on an
// inventory that mis-declared — run with no isolation at all, the fail-open
// shape LEDGER L-107 found once already (readOnlyRootFilesystem silently
// inert on Windows). The instance loader refuses the same effects on a
// Windows runner entry and the dispatcher re-asserts at pod render; the
// three sites read one predicate.
//
// Only KNOWN effects are judged (an unknown token is already the vocabulary
// error above). A floor restriction under a Windows gaggle OS is reported
// once at the gaggle; the per-stage walk covers everything else, including
// a floor restriction meeting a stage-declared windows OS.
func windowsRestrictionProblems(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	var problems []string
	reportedAtGaggle := map[string]bool{}
	if gaggleRunsOn != nil && gaggleRunsOn.OS == runnersolve.OSWindows {
		for _, token := range gaggleRunsOn.Restrictions {
			if !runnercap.KnownRestriction(token) || runnercap.DeclarableOnWindows(runnercap.Restriction(token)) {
				continue
			}
			reportedAtGaggle[token] = true
			problems = append(problems, fmt.Sprintf("gaggle runsOn.restrictions: %s", windowsUndeclarableRestriction(token)))
		}
	}
	for _, stage := range runsOnStages(def) {
		effective := EffectiveRunsOn(stage, gaggleRunsOn)
		if effective.OS != runnersolve.OSWindows {
			continue
		}
		for _, token := range effective.Restrictions {
			if reportedAtGaggle[token] || !runnercap.KnownRestriction(token) || runnercap.DeclarableOnWindows(runnercap.Restriction(token)) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s %q runsOn.restrictions: %s", stage.Kind, stage.Name, windowsUndeclarableRestriction(token)))
		}
	}
	return problems
}

func windowsUndeclarableRestriction(token string) string {
	declarable := runnercap.WindowsDeclarableRestrictions()
	names := make([]string, 0, len(declarable))
	for _, r := range declarable {
		names = append(names, string(r))
	}
	return fmt.Sprintf("%q has no Windows binding in v1 — a stage whose effective runsOn.os is windows may require only %s; Kubernetes ignores readOnlyRootFilesystem on a Windows pod and the network effects await the Windows sandboxing epic (goobernetes-restrictions.md D4/D11)",
		token, strings.Join(names, ", "))
}

func unknownRestriction(token string) string {
	names := make([]string, 0, len(runnercap.KnownRestrictions()))
	for _, r := range runnercap.KnownRestrictions() {
		names = append(names, string(r))
	}
	message := fmt.Sprintf("unknown restriction %q (the closed v1 effect list: %s)", token, strings.Join(names, ", "))
	if suggestion, ok := runnercap.SuggestRestriction(token); ok {
		message += fmt.Sprintf(" (did you mean %q?)", suggestion)
	}
	return message
}

// EffectiveRunsOn resolves a stage's effective placement requirement: the
// gaggle-level floor merged with the stage's own declaration (capabilities
// and restrictions union, OS from whichever side declares one — a conflict is
// already a compile error via runsOnProblems). Quantities come from the stage
// alone; the gaggle floor has none by design. The stage is a task or an
// agentic gate (PlacementStage): the merge rule is one rule, not two.
func EffectiveRunsOn(stage PlacementStage, gaggleRunsOn *apiv1.GaggleRunsOn) apiv1.RunsOn {
	var effective apiv1.RunsOn
	if stage.RunsOn != nil {
		effective = *stage.RunsOn
		effective.Capabilities = append([]string(nil), stage.RunsOn.Capabilities...)
		effective.Restrictions = append([]string(nil), stage.RunsOn.Restrictions...)
	}
	if gaggleRunsOn != nil {
		if effective.OS == "" {
			effective.OS = gaggleRunsOn.OS
		}
		effective.Capabilities = append(effective.Capabilities, gaggleRunsOn.Capabilities...)
		effective.Restrictions = append(effective.Restrictions, gaggleRunsOn.Restrictions...)
	}
	effective.Capabilities = sortedDistinct(effective.Capabilities)
	effective.Restrictions = sortedDistinct(effective.Restrictions)
	return effective
}
