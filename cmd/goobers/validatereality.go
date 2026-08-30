package main

// validatereality.go teaches `goobers validate` the blind spots the
// 2026-08-08 cold-start exercise proved certify never-working configs:
//
//   - static (every validate run): a gaggle/stage requiredCapabilities token
//     no instance runner claims (CAP003 — dotnet #7, swift probes), and an
//     unenforceable workflow maxOpenPRs cap (PRCAP001), and an
//     automated gate's failure-keyed branch that declares completion a
//     failed-without-continueOnError stage can never reach as a completed
//     run (WF018 — swift #3's shape, corrected against the runner's actual
//     semantics; see appendGateCompletionWarnings).
//
//   - network (--check-repos only, after the repository preflight already
//     contacted the repo): the repository's actual label set compared
//     against every selector and stage-applied label (SELECTOR001..003 —
//     python #1/#7, swift #10), the combined positive selector's live
//     open-item match count (SELECTOR002), and a ci-poll workflow pointed
//     at a repository whose routed credential cannot read CI workflows or
//     which has no CI workflows (CIPOLL001 — swift #9 + probe).
//
// Everything here is advisory: static findings are ordinary config warnings
// (they count under --strict like any other config warning), network
// findings are repo-state observations that print and land in the JSON
// diagnostics envelope but never change the exit code — repo state can
// change a minute later, and --strict must not fail a CI runner over it
// (same contract as #1547's oversized-repo warning).

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
	"github.com/goobers/goobers/internal/runnersolve"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

// -----------------------------------------------------------------------------
// Static checks (no network): capability, readiness, and gate cross-checks.
// -----------------------------------------------------------------------------

// proberFamilies mirrors internal/toolchain.DefaultVerifier's registered probe
// families (the source of truth — a new prober there should be added here
// too). A required token outside every family is still schedule-time matched
// (exact string membership against runner.capabilities, internal/runnercap),
// but the host toolchain is never probed for it, so a typo'd family is
// undetectable at runtime — worth naming in the diagnostic.
var proberFamilies = map[string]bool{
	"dotnet": true,
	"go":     true,
	"java":   true,
	"node":   true,
	"os":     true,
	"python": true,
}

// proberFamilyList is proberFamilies rendered for diagnostics, sorted.
func proberFamilyList() string {
	families := make([]string, 0, len(proberFamilies))
	for family := range proberFamilies {
		families = append(families, family)
	}
	sort.Strings(families)
	return strings.Join(families, ", ")
}

// capabilityTokenFamily splits a runner-capability token into its family the
// same way internal/toolchain.splitToken does: the text before the first `@`
// (dotnet@8) or `=` (os=windows); a bare token (xcode) is its own family.
func capabilityTokenFamily(token string) string {
	if i := strings.IndexAny(token, "@="); i >= 0 {
		return token[:i]
	}
	return token
}

// realityWarning pairs a coded warning with the diagnostics-envelope location
// its producer resolved (the offending file plus a JSON-pointer-ish path),
// mirroring how appendGooberHarnessWarnings' caller attributes its findings.
type realityWarning struct {
	warning validate.CodedWarning
	file    string
	path    string
}

// appendStaticRealityWarnings runs the static cross-file reality checks —
// the per-stage placement solve against the resolved runner inventory
// (RNR001/RNR003/RNR004, the shared solver of dsl-3.0.md §5 checkpoint 1),
// the legacy instance.yaml runner claims vs config requiredCapabilities
// cross-check (CAP003), and gate completion-branch reachability — appending
// each finding to report (so --strict and the JSON report see it like any
// other config finding) and returning the findings with their file/path
// attribution for the diagnostics collector. Placement findings carry ERROR
// severity when a runners: inventory is declared (the #3497 fix; the caller
// fails validation on them) unless advisory is set, which happens for two
// callers: `goobers validate --source-tree` has no real instance.yaml (it
// substitutes instance.yaml.example, so its solve is advisory by
// definition), and the daemon's startup preflight, where the finding's
// consequence is a per-workflow refusal rather than a dead config
// (checkpoint 3, #2860 — see runStartupConfigPreflight).
func appendStaticRealityWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	report *validate.Report,
	advisory bool,
) []realityWarning {
	if set == nil || report == nil {
		return nil
	}
	var warnings []realityWarning
	addSeverity := func(code validate.WarningCode, severity validate.Severity, kind, name, file, path, message string) {
		report.Issues = append(report.Issues, validate.Issue{
			Code:     code,
			Severity: severity,
			Kind:     kind,
			Name:     name,
			Message:  message,
		})
		warnings = append(warnings, realityWarning{
			warning: validate.CodedWarning{
				Code:        code,
				Severity:    severity,
				Scope:       kind + "/" + name,
				Explanation: message,
			},
			file: file,
			path: path,
		})
	}
	add := func(code validate.WarningCode, kind, name, file, path, message string) {
		addSeverity(code, validate.Warning, kind, name, file, path, message)
	}
	appendPlacementFindings(root, configDir, cfg, set, goobers, advisory, addSeverity)
	appendUnclaimedCapabilityWarnings(root, configDir, cfg, set, add)
	appendMaxOpenPRWarnings(root, configDir, cfg, set, add)
	appendGateCompletionWarnings(root, configDir, set, add)
	appendWindowsAVExclusionWarnings(root, cfg, add)
	return warnings
}

// appendWindowsAVExclusionWarnings is #3480's declaration half (RNR006): a
// runners: entry that claims provides.os: windows but does not assert
// provides.windows.avExclusionsVerified: true. Always a warning — the
// claim is trusted, never verified (DI-11), and the condition it guards is
// a write-then-read race against real-time scanning that shows up as an
// unrelated git "Permission denied" (#3161–#3164), not a wrong result. An
// explicit false is reported in its own words: it is an honest answer, and
// the operator should see that the runner is known-unprepared rather than
// merely undeclared.
func appendWindowsAVExclusionWarnings(
	root string,
	cfg *instance.Config,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil {
		return
	}
	file := diagnosticFile(root, filepath.Join(root, instance.ConfigFileName))
	for i, entry := range cfg.Runners {
		if entry.Provides.OS != instance.RunnerOSWindows {
			continue
		}
		if entry.Provides.Windows != nil && entry.Provides.Windows.AVExclusionsVerified {
			continue
		}
		state := "does not declare provides.windows.avExclusionsVerified"
		if entry.Provides.Windows != nil {
			state = "declares provides.windows.avExclusionsVerified: false"
		}
		add(validate.RunnerAVExclusionsUnverified, "Instance", "runners["+entry.Name+"]", file,
			fmt.Sprintf("/runners/%d/provides/windows/avExclusionsVerified", i),
			fmt.Sprintf("runner %q declares provides.os: windows and %s — whether the directories Goobers writes then "+
				"immediately reads on it are excluded from real-time antivirus scanning is unknown; a scan holding a handle "+
				"on a just-written file surfaces later as an unrelated git \"Permission denied\" (#3480). Run `goobers doctor "+
				"--av-exclusions` on the runner's host or image and declare the result (the claim is trusted, never verified — "+
				"the stage pod's own startup advisory line is the evidence)", entry.Name, state))
	}
}

// appendPlacementFindings is checkpoint 1 of the three-checkpoint admission
// (dsl-3.0.md §5, decision record D4): the full per-stage constraint solve —
// stages × runners over os, quantities, capabilities, and restrictions —
// run by the SAME implementation the daemon's boot pass and the scheduler's
// per-run admit consume (internal/runnersolve; the CAP003/scheduler mirror
// lesson: a second implementation diverges into configs that validate but
// never schedule).
//
// Severity: RNR001/RNR003 are errors iff a runners: inventory is declared
// (closing the #3497 exit-0 trap for declared inventories) and warnings
// otherwise; RNR004 is always a warning (local-mode resource minimums are
// advisory, dsl-3.0.md D4). Workflow scope mirrors the daemon: with a
// declared inventory every workflow is solved; on an inventory-less
// instance only 3.0 documents are solved here — 2.0 documents keep CAP003's
// frozen per-gaggle warning below (frozen interpreter, frozen severity), so
// a zero-declaration instance's validation output is byte-identical for
// them.
func appendPlacementFindings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	advisory bool,
	add func(code validate.WarningCode, severity validate.Severity, kind, name, file, path, message string),
) {
	if cfg == nil {
		return
	}
	inventoryDeclared := len(cfg.Runners) > 0
	unsatSeverity := validate.Warning
	if inventoryDeclared && !advisory {
		unsatSeverity = validate.Error
	}
	// selfOS is "" here — NEVER runnersolve.HostOS(): checkpoint 1 must be
	// machine-independent (the same config validates identically, same exit
	// code and findings, on every GOOS — the validating machine's OS says
	// nothing about the daemon host the config will run on). A self entry
	// with no declared provides.os is os-UNKNOWN at validate time; the
	// HostOS substitution is runtime-only (boot/admission on the actual
	// executing host — see instance.Config.PlacementRunners).
	inventory := runnersolve.Inventory{Runners: cfg.PlacementRunners("")}
	// selfRunnerNames is every inventory entry the solver treats as the
	// daemon host (runnersolve.Runner.Self — a self entry's Name is author-
	// chosen, not necessarily the literal "self": internal/instance's
	// PlacementRunners copies entry.Name verbatim and sets Self from
	// entry.Host alone). RNR005 below tests eligible-set membership against
	// this set, never the literal string "self".
	selfRunnerNames := make(map[string]bool, len(inventory.Runners))
	for _, r := range inventory.Runners {
		if r.Self {
			selfRunnerNames[r.Name] = true
		}
	}
	gaggleSpecs := make(map[string]apiv1.GaggleSpec, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleSpecs[set.Gaggles[i].Name] = set.Gaggles[i].Spec
	}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		if !inventoryDeclared && wf.DSLVersion != supportmatrix.V3DSLVersion {
			continue
		}
		requirements, err := workflow.StagePlacements(workflow.Definition{
			Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
		}, gaggleSpecs[wf.Spec.Gaggle], goobers)
		if err != nil {
			// An unresolvable dslVersion has already failed validation in the
			// compile pass; nothing to solve here.
			continue
		}
		source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		file := configSourceDiagnosticFile(root, configDir, source)
		taskIndex := make(map[string]int, len(wf.Spec.Tasks))
		for ti := range wf.Spec.Tasks {
			taskIndex[wf.Spec.Tasks[ti].Name] = ti
		}
		// A solver row can name an agentic gate that declares runsOn
		// (decision 001); attribute it to the gate's own block.
		gateIndex := make(map[string]int, len(wf.Spec.Gates))
		for gi := range wf.Spec.Gates {
			gateIndex[wf.Spec.Gates[gi].Name] = gi
		}
		pathFor := func(stage string) string {
			if ti, ok := taskIndex[stage]; ok {
				return fmt.Sprintf("/spec/tasks/%d/runsOn", ti)
			}
			if gi, ok := gateIndex[stage]; ok {
				return fmt.Sprintf("/spec/gates/%d/runsOn", gi)
			}
			return "/spec/tasks"
		}
		requirementFor := make(map[string]runnersolve.StageRequirement, len(requirements))
		for _, requirement := range requirements {
			requirementFor[requirement.Stage] = requirement
		}
		result := runnersolve.Solve(inventory, requirements)
		for _, placement := range result.Stages {
			for _, note := range placement.Advisories {
				add(validate.RunnerQuantityAdvisory, validate.Warning, "Workflow", wf.Name,
					file, pathFor(placement.Stage), note.Diagnostic)
			}
			if ti, ok := taskIndex[placement.Stage]; ok {
				appendInstanceRootFinding(&wf.Spec.Tasks[ti], placement, selfRunnerNames,
					wf.Name, file, pathFor(placement.Stage), add)
			}
			if placement.Unsat == nil {
				continue
			}
			code := validate.RunnerStageUnsatisfiable
			remedy := "declare the requirement on a runner that provides it, or relax the stage's runsOn"
			if placement.Unsat.Kind == runnersolve.UnsatQuantity {
				code = validate.RunnerQuantityUnsatisfiable
				remedy = "raise a runner's declared ceiling or lower the stage minimum"
			}
			severity := unsatSeverity
			message := placement.Unsat.Diagnostic
			switch {
			case selfOSUnknownUnsat(inventory, requirementFor[placement.Stage], placement):
				// The ONLY reason this stage is unsatisfiable is that the self
				// runner's OS is unknown at validate time (no declared
				// provides.os; the stage would place on self if its OS
				// matched). Whether it actually places is a runtime fact of
				// the daemon host, which boot/admission check authoritatively
				// with the real host OS — so this is a machine-independent
				// WARNING with guidance, never an exit-code-changing error
				// that would flip with the validating machine's GOOS.
				severity = validate.Warning
				message += "; the self runner declares no provides.os, so its operating system is unknown at validate time and this check stays machine-independent — declare provides.os on the self runner to validate OS placement statically, or rely on runtime admission (the daemon checks the actual host OS at boot and per run)"
			case inventoryDeclared:
				message += "; no run of this workflow can be scheduled on the declared inventory (" + remedy + ")"
			default:
				message += "; advisory on this inventory-less instance — the local runner admits runs against its claimed capabilities at schedule time (declare a runners: inventory to enforce placement here)"
			}
			add(code, severity, "Workflow", wf.Name, file, pathFor(placement.Stage), message)
		}
	}
}

// appendInstanceRootFinding is decision 003 ruling 3's static advance-notice
// half of the refusal that actually lives at dispatch
// (executor.StageRequiresInstanceRoot, consumed by internal/engine's
// dispatchRemoteTask before a pod is ever created): it warns on a 3.0 stage
// whose resolved ELIGIBLE RUNNER SET excludes every self entry, but whose
// command or built-in stage kind needs the daemon's instance root — the
// file claim ledger, a merge lock, an on-disk run journal, or a kind with
// no pod-side execution path (ci-poll, external-telemetry).
//
// Eligibility comes from the exact solve appendPlacementFindings just ran
// for placement, reused here rather than re-derived, so this can never
// disagree with it the way inferring off-self-ness from a bare
// "runsOn.restrictions is non-empty" check could: a `host: self` inventory
// entry MAY declare restrictions (self enforces only what it declares,
// never implicitly — the appendPlacementFindings comment above states the
// same invariant, runnersolve.go's `enforces`), and when it declares the
// ones a stage requires, self stays eligible and this must NOT warn.
//
// A bare runsOn with no restrictions is NOT flagged: self trivially
// satisfies an empty requirement and so is always in the eligible set,
// exactly like every other stage with no runsOn at all.
//
// Always a WARNING (RNR005), never promoted the way RNR001/RNR003 are on a
// declared inventory: dispatch is the enforcement — a placed run of this
// workflow is refused loud with the same named code, never silently wrong —
// so this is advance notice at author time, not a second gate.
func appendInstanceRootFinding(
	task *apiv1.Task, placement runnersolve.StagePlacement, selfRunnerNames map[string]bool,
	workflowName, file, path string,
	add func(code validate.WarningCode, severity validate.Severity, kind, name, file, path, message string),
) {
	if task.Type != apiv1.TaskDeterministic || task.Run == nil {
		return
	}
	kind := strings.TrimSpace(task.Inputs[executor.InputKind])
	if !executor.StageRequiresInstanceRoot(task.Run.Command, kind) {
		return
	}
	if placementIncludesSelf(placement.Eligible, selfRunnerNames) {
		return
	}
	why := fmt.Sprintf("command %v", task.Run.Command)
	if kind != "" && kind != executor.KindShell {
		why = fmt.Sprintf("inputs.kind=%q", kind)
	}
	add(validate.RunnerInstanceRootRequired, validate.Warning, "Workflow", workflowName, file, path,
		fmt.Sprintf(
			"cannot resolve to the daemon's own host (self) — its %s needs the daemon's instance root (the file claim ledger, a merge lock, an on-disk run journal, or a built-in stage kind with no pod-side execution path), but the eligible runner set for this stage is %v; it will be refused at dispatch (decision 003, code %q) — declare the requirement(s) this stage's runsOn needs on the self runner entry, if it genuinely provides them, or restructure the workflow so this stage's command runs on a stage that resolves to self",
			why, placement.Eligible, executor.StageRequiresInstanceRootCode,
		))
}

// placementIncludesSelf reports whether any name in eligible names a self
// runner (see selfRunnerNames in appendPlacementFindings) — never checked
// against the literal string "self", since a self entry's Name is
// author-chosen (internal/instance.PlacementRunners).
func placementIncludesSelf(eligible []string, selfRunnerNames map[string]bool) bool {
	for _, name := range eligible {
		if selfRunnerNames[name] {
			return true
		}
	}
	return false
}

// selfOSUnknownUnsat reports whether a stage's unsatisfiability is
// attributable solely to the self runner's UNKNOWN OS at validate time: the
// stage requires an OS, the inventory has a self entry with no declared
// provides.os, and re-solving with that entry assumed to claim the required
// OS makes the stage satisfiable. Such a finding is a runtime fact of the
// daemon host (boot/admission substitute the real host OS), so checkpoint 1
// reports it at warning severity — keeping validate's exit code identical
// on every GOOS.
func selfOSUnknownUnsat(inventory runnersolve.Inventory, requirement runnersolve.StageRequirement, placement runnersolve.StagePlacement) bool {
	if placement.Unsat == nil || placement.Unsat.Kind != runnersolve.UnsatRequirement || requirement.OS == "" {
		return false
	}
	assumed := runnersolve.Inventory{Mandates: inventory.Mandates}
	sawUnknownSelf := false
	for _, runner := range inventory.Runners {
		if runner.Self && runner.OS == "" {
			runner.OS = requirement.OS
			sawUnknownSelf = true
		}
		assumed.Runners = append(assumed.Runners, runner)
	}
	if !sawUnknownSelf {
		return false
	}
	return len(runnersolve.Solve(assumed, []runnersolve.StageRequirement{requirement}).Unsatisfiable()) == 0
}

// appendUnclaimedCapabilityWarnings cross-checks every gaggle's whole-gaggle
// runner-capability union (gaggle spec.requiredCapabilities plus every bound
// pre-3.0 workflow stage's requiredCapabilities) against the instance
// runner's claimed set, using the scheduler's own matching primitive
// (runnercap.NewClaimed/Missing: exact string set membership, never a
// version range) so this check can never disagree with schedule-time
// matching. An unclaimed token today validates clean and then refuses every
// run: the scheduler's admit check skips the workflow each tick
// (RRQ-1/#1101) — the cold-start dotnet #7 / swift `swift@9.9` +
// `totally-made-up-toolchain@42` probes.
//
// Scope, per dsl-3.0.md §5: CAP003 keeps its shipped meaning for 2.0
// documents on inventory-less instances ONLY (frozen interpreter, frozen
// severity — zero-declaration invariance). With a declared runners:
// inventory this whole-gaggle self-claims union would both mis-warn about
// capabilities another runner provides and duplicate the solver's findings,
// so the per-stage placement solve (appendPlacementFindings, RNR001 at
// error severity) replaces it entirely there; 3.0 documents and a gaggle
// declaring a runsOn floor are likewise the solver's, on every instance
// shape.
func appendUnclaimedCapabilityWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil || len(cfg.Runners) > 0 {
		return
	}
	pre30Workflows := make([]apiv1.Workflow, 0, len(set.Workflows))
	for i := range set.Workflows {
		if set.Workflows[i].DSLVersion != supportmatrix.V3DSLVersion {
			pre30Workflows = append(pre30Workflows, set.Workflows[i])
		}
	}
	claimed := runnercap.NewClaimed(cfg.SelfRunnerCapabilities())
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i]
		if gaggle.Spec.RunsOn != nil {
			// A runsOn floor pairs only with 3.0 workflows (the compile-time
			// rule of dsl-3.0.md open point 2); its solve lives in RNR001.
			continue
		}
		required := instance.RequiredCapabilities(gaggle, pre30Workflows)
		for _, token := range claimed.Missing(required) {
			// The consequence clause matches the shipped behavior after #2936
			// (#2860's ruling): the daemon starts, and each affected run is
			// refused at schedule time — no boot-kill to warn about.
			message := fmt.Sprintf(
				"requires runner capability %q, but runner.capabilities in instance.yaml does not claim it, "+
					"so the scheduler refuses to place every run of this gaggle at schedule time; "+
					"add %q to runner.capabilities (schedule-time matching is an exact string match)",
				token, token)
			if family := capabilityTokenFamily(token); !proberFamilies[family] {
				message += fmt.Sprintf(
					" — note %q is outside the prober families (%s), so the host toolchain is never verified for it; "+
						"double-check the token spelling", family, proberFamilyList())
			}
			add(validate.WarningUnclaimedRunnerCapability, "Gaggle", gaggle.Name,
				gaggleDiagnosticFile(root, configDir, set, gaggle.Name),
				"/spec/requiredCapabilities", message)
		}
	}
}

func appendMaxOpenPRWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil {
		return
	}
	projects := make(map[string]apiv1.RepoRef, len(set.Gaggles))
	for i := range set.Gaggles {
		projects[set.Gaggles[i].Name] = set.Gaggles[i].Spec.Project
	}
	for i := range set.Workflows {
		workflow := &set.Workflows[i]
		if workflow.Spec.Readiness.MaxOpenPRs <= 0 {
			continue
		}
		project, ok := projects[workflow.Spec.Gaggle]
		if !ok {
			continue
		}
		var message string
		switch {
		case project.Owner == "" && project.Name == "" && len(cfg.Repos) > 0 && cfg.Repos[0].Provider == string(apiv1.ProviderADO):
			message = fmt.Sprintf(
				"readiness.maxOpenPRs cannot be enforced for ADO project repository %q: "+
					"the cap counts GitHub pull requests, so no open-PR count is available and admission fails open",
				instanceRepoName(cfg.Repos[0]))
		case project.Owner == "" && project.Name == "" && len(cfg.Repos) > 0:
			message = fmt.Sprintf(
				"readiness.maxOpenPRs has no project repository binding, so the cap binds to instance repos[0] repository %q",
				instanceRepoName(cfg.Repos[0]))
		case project.Provider == apiv1.ProviderADO:
			message = fmt.Sprintf(
				"readiness.maxOpenPRs cannot be enforced for ADO project repository %q: "+
					"the cap counts GitHub pull requests, so no open-PR count is available and admission fails open",
				projectRepoName(project))
		case project.Provider == apiv1.ProviderGitHub:
			if _, configured := configuredRepoForProject(cfg, project); configured {
				continue
			}
			message = fmt.Sprintf(
				"readiness.maxOpenPRs binds to project repository %q, but instance.yaml has no configured binding for that repository; "+
					"its polling credential cannot be resolved, so the open-PR count remains unknown and admission fails open",
				projectRepoName(project))
		default:
			continue
		}
		source, _ := set.WorkflowSource(workflow.Spec.Gaggle, workflow.Name)
		add(validate.WarningMaxOpenPRsUnenforceable, "Workflow", workflow.Name,
			configSourceDiagnosticFile(root, configDir, source),
			"/spec/readiness/maxOpenPRs", message)
	}
}

func projectRepoName(project apiv1.RepoRef) string {
	if project.Provider == apiv1.ProviderADO {
		return strings.Join([]string{string(project.Provider), project.Owner, project.Project, project.Name}, "/")
	}
	return strings.Join([]string{string(project.Provider), project.Owner, project.Name}, "/")
}

// gateFailureKeyedOutcomes returns the outcomes of an automated gate that
// imply the subject stage's own status was "failure", per
// internal/gate.DefaultChecks: status-equals resolves "fail" exactly when
// Inputs[status] != equals (default "success"), and failure-class resolves
// "fail"/"infra" only for a non-success status. Output-driven checks
// (output-equals, ci-status, land-outcome, …) key on stage outputs, not the
// stage's status, so their "fail" branches are legitimately reachable from a
// succeeded stage and are out of scope here.
func gateFailureKeyedOutcomes(gate apiv1.Gate) []string {
	if gate.Evaluator != apiv1.EvaluatorAutomated || gate.Automated == nil {
		return nil
	}
	switch gate.Automated.Check {
	case "status-equals":
		want := gate.Automated.Params["equals"]
		if want == "" || want == string(apiv1.ResultSuccess) {
			return []string{"fail"}
		}
		return nil
	case "failure-class":
		return []string{"fail", "infra"}
	default:
		return nil
	}
}

// appendGateCompletionWarnings flags the one continueOnError-shaped dead
// branch the runner actually has. The cold-start swift #3 ledger claimed a
// gate `fail:` branch is unreachable whenever the preceding deterministic
// stage omits continueOnError; the runner's real semantics disagree — a
// failed stage whose `next` names a gate ALWAYS delivers its failed status
// to that gate, continueOnError or not (internal/runner/run.go taskOutcome;
// proven by TestRunnerTaskFailureWithGateNextStillBranches), which is why
// the shipped references that omit it are correct. What IS dead is the
// narrower shape verified by TestRunnerTaskFailureContinueOnErrorMatrix's
// "default gate preserves unresolved failure" case: when the failure-keyed
// branch routes to workflow COMPLETION (""), the run still terminates
// failed, because gateTransition (#849) refuses to complete a run whose
// final stage failure was neither tolerated (continueOnError) nor cleared
// by a pass/human verdict. The declared completion is unreachable; the
// one-line fix is continueOnError: true on the feeding stage.
func appendGateCompletionWarnings(
	root, configDir string,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		file := configSourceDiagnosticFile(root, configDir, source)
		for gateIndex, gate := range wf.Spec.Gates {
			var deadOutcomes []string
			for _, outcome := range gateFailureKeyedOutcomes(gate) {
				if target, declared := gate.Branches[outcome]; declared && target == "" {
					deadOutcomes = append(deadOutcomes, outcome)
				}
			}
			if len(deadOutcomes) == 0 {
				continue
			}
			for _, task := range wf.Spec.Tasks {
				if task.Next != gate.Name || task.ContinueOnError {
					continue
				}
				for _, outcome := range deadOutcomes {
					message := fmt.Sprintf(
						"gate %q branch %q routes a failed %q result to workflow completion, but stage %q does not set "+
							"continueOnError, so every run taking that branch terminates failed instead of completed "+
							"(the runner only completes through a failure it was told to tolerate); "+
							"set continueOnError: true on stage %q, or route the branch to a parking stage or terminal (@abort/@escalate)",
						gate.Name, outcome, task.Name, task.Name, task.Name)
					add(validate.WarningGateCompletionHidesFailure, "Workflow", wf.Name, file,
						fmt.Sprintf("/spec/gates/%d/branches/%s", gateIndex, outcome), message)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Network checks (--check-repos): selector/label reality and CI reality.
// -----------------------------------------------------------------------------

// labelUseKind classifies how the config references a repository label, which
// decides the consequence clause the warning carries.
type labelUseKind int

const (
	// labelUseSelect: a positive selector reference (backlog labels,
	// trustLabel, requireLabels, backlog-item trigger selector keys) — a
	// nonexistent label can never match, so the loop claims nothing.
	labelUseSelect labelUseKind = iota
	// labelUseExclude: an excludeLabels reference — a nonexistent label
	// never excludes anything; almost always a vocabulary typo.
	labelUseExclude
	// labelUseApply: a label a shipped stage will APPLY (backlog-query
	// --claim's claim mirror, issue-close-out's park/close-out statuses) —
	// GitHub rejects applying labels that do not exist, so the first
	// park/close-out fails mid-run.
	labelUseApply
)

// labelUse is one config reference to a repository label.
type labelUse struct {
	label string
	kind  labelUseKind
	// where names the referencing config location for the message, e.g.
	// `Gaggle/example spec.backlog.labels` or
	// `Workflow/default-implement task "query-backlog" inputs.trustLabel`.
	where string
	// file/path attribute the diagnostic to the offending source.
	file string
	path string
}

// gaggleSelectorQuery is one gaggle's combined positive selector — derived by
// repoSelectorLabels (cmd/goobers/repolabels.go), the single shared
// definition `goobers connect`'s reality echo also uses, so the two surfaces
// can never disagree — whose live open-item match count the zero-work check
// probes (SELECTOR002).
type gaggleSelectorQuery struct {
	gaggle string
	labels []string
	file   string
	path   string
}

// ciPollUse is one ci-poll-kind stage bound to the repository.
type ciPollUse struct {
	workflow string
	stage    string
	file     string
	path     string
}

// repoRealityDemand is everything the config demands of one instance
// repository, gathered statically so the network pass fetches each fact at
// most once per repository.
type repoRealityDemand struct {
	labelUses []labelUse
	selectors []gaggleSelectorQuery
	ciPoll    []ciPollUse
}

func (d repoRealityDemand) empty() bool {
	return len(d.labelUses) == 0 && len(d.selectors) == 0 && len(d.ciPoll) == 0
}

// isGoobersStageCommand reports whether a deterministic task shells out to
// the named goobers CLI verb, mirroring defaultBacklogQueryAssignedTo's
// recognition (filepath.Base tolerates an absolute goobers path).
func isGoobersStageCommand(task apiv1.Task, verb string) bool {
	return task.Type == apiv1.TaskDeterministic && task.Run != nil &&
		len(task.Run.Command) >= 2 &&
		filepath.Base(task.Run.Command[0]) == "goobers" &&
		task.Run.Command[1] == verb
}

func stageCommandHasFlag(task apiv1.Task, flag string) bool {
	if task.Run == nil {
		return false
	}
	for _, arg := range task.Run.Command {
		if arg == flag {
			return true
		}
	}
	return false
}

// issueCloseOutAppliedLabel maps an issue-close-out stage's declared status
// input to the GitHub label that stage applies: park statuses add their park
// label (cmd/goobers/issuecloseout.go), and in-review mirrors the
// goobers/status: processing label (providers.UpdateWorkItemStatus). done and
// the empty default close the issue without needing a new label, so they
// return "".
func issueCloseOutAppliedLabel(status string) string {
	switch providers.WorkItemStatus(status) {
	case issueCloseOutNeedsHuman:
		return providers.LabelNeedsHuman
	case issueCloseOutNeedsRemediation:
		return needsRemediationLabel
	case providers.WorkItemStatusInReview:
		return "goobers/status:in-review"
	default:
		return ""
	}
}

// gatherRepoRealityDemand walks the config set and maps every selector
// reference, stage-applied label, and ci-poll stage onto the instance repo
// index it is bound to (same binding rules as checkGaggleRepositoryBindings:
// exact provider/owner/project/name match, or the single-repo empty-project
// default). Gaggles that bind to no configured repo are skipped — REPO002
// already fails those.
func gatherRepoRealityDemand(root, configDir string, cfg *instance.Config, set *instance.ConfigSet) map[int]*repoRealityDemand {
	demand := map[int]*repoRealityDemand{}
	if cfg == nil || set == nil {
		return demand
	}
	repoIndex := func(project apiv1.RepoRef) (int, bool) {
		bound, ok := configuredRepoForProject(cfg, project)
		if !ok {
			return 0, false
		}
		for i, repo := range cfg.Repos {
			if repo.Provider == bound.Provider && repo.Owner == bound.Owner &&
				repo.Project == bound.Project && repo.Name == bound.Name {
				return i, true
			}
		}
		return 0, false
	}
	at := func(i int) *repoRealityDemand {
		if demand[i] == nil {
			demand[i] = &repoRealityDemand{}
		}
		return demand[i]
	}

	for gi := range set.Gaggles {
		gaggle := set.Gaggles[gi]
		index, bound := repoIndex(gaggle.Spec.Project)
		if !bound {
			continue
		}
		d := at(index)
		gaggleFile := gaggleDiagnosticFile(root, configDir, set, gaggle.Name)
		addUse := func(label string, kind labelUseKind, where, file, path string) {
			label = strings.TrimSpace(label)
			if label == "" {
				return
			}
			d.labelUses = append(d.labelUses, labelUse{label: label, kind: kind, where: where, file: file, path: path})
		}

		for _, label := range gaggle.Spec.Backlog.Labels {
			addUse(label, labelUseSelect,
				fmt.Sprintf("Gaggle/%s spec.backlog.labels", gaggle.Name), gaggleFile, "/spec/backlog/labels")
		}
		for _, label := range gaggle.Spec.RequireLabels {
			addUse(label, labelUseSelect,
				fmt.Sprintf("Gaggle/%s spec.requireLabels", gaggle.Name), gaggleFile, "/spec/requireLabels")
		}
		// The zero-work probe uses the gaggle's combined positive selector as
		// repoSelectorLabels derives it — the same derivation `goobers
		// connect`'s reality echo (CONNECT004) and `connect --seed` use.
		if combined := repoSelectorLabels(gaggle, set.Workflows); len(combined) > 0 {
			d.selectors = append(d.selectors, gaggleSelectorQuery{
				gaggle: gaggle.Name,
				labels: combined,
				file:   gaggleFile,
				path:   "/spec/backlog",
			})
		}

		for wi := range set.Workflows {
			wf := &set.Workflows[wi]
			if wf.Spec.Gaggle != gaggle.Name {
				continue
			}
			source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
			wfFile := configSourceDiagnosticFile(root, configDir, source)

			for ti, trigger := range wf.Spec.Triggers {
				if trigger.Type != apiv1.TriggerBacklogItem {
					continue
				}
				path := fmt.Sprintf("/spec/triggers/%d", ti)
				where := fmt.Sprintf("Workflow/%s backlog-item trigger", wf.Name)
				for key := range trigger.Selector {
					addUse(key, labelUseSelect, where+" selector", wfFile, path+"/selector")
				}
				if trust := strings.TrimSpace(trigger.TrustLabel); trust != "" {
					addUse(trust, labelUseSelect, where+" trustLabel", wfFile, path+"/trustLabel")
				}
			}

			for ti, task := range wf.Spec.Tasks {
				taskPath := fmt.Sprintf("/spec/tasks/%d/inputs", ti)
				taskWhere := fmt.Sprintf("Workflow/%s task %q", wf.Name, task.Name)

				if kind := strings.TrimSpace(task.Inputs[executor.InputKind]); kind == executor.KindCIPoll {
					d.ciPoll = append(d.ciPoll, ciPollUse{
						workflow: wf.Name, stage: task.Name, file: wfFile,
						path: fmt.Sprintf("/spec/tasks/%d", ti),
					})
				}

				if trust := strings.TrimSpace(task.Inputs["trustLabel"]); trust != "" {
					addUse(trust, labelUseSelect, taskWhere+" inputs.trustLabel", wfFile, taskPath+"/trustLabel")
				}
				// A task with no requireLabels of its own inherits the gaggle
				// default (defaultBacklogQueryRequireLabels — full replace,
				// never merged); the default's existence warning is already
				// attributed to the gaggle above.
				if require, declared := task.Inputs["requireLabels"]; declared {
					for _, label := range splitLabelList(require) {
						addUse(label, labelUseSelect, taskWhere+" inputs.requireLabels", wfFile, taskPath+"/requireLabels")
					}
				}
				for _, label := range splitLabelList(task.Inputs["excludeLabels"]) {
					addUse(label, labelUseExclude, taskWhere+" inputs.excludeLabels", wfFile, taskPath+"/excludeLabels")
				}

				if isGoobersStageCommand(task, "backlog-query") && stageCommandHasFlag(task, "--claim") {
					addUse(providers.LabelClaimed, labelUseApply,
						taskWhere+" (--claim's claim mirror)", wfFile, fmt.Sprintf("/spec/tasks/%d/run/command", ti))
				}

				if isGoobersStageCommand(task, "issue-close-out") {
					if applied := issueCloseOutAppliedLabel(task.Inputs["status"]); applied != "" {
						addUse(applied, labelUseApply,
							fmt.Sprintf("%s inputs.status=%q", taskWhere, task.Inputs["status"]),
							wfFile, taskPath+"/status")
					}
				}
			}
		}
	}
	return demand
}

// The network fetch seams, overridable in tests exactly like
// targetRepositoryReachable/targetRepositorySize above them in validate.go.
// validateRealityLister builds the read-only work-item lister the zero-work
// probe hands to checkRepoSelectorReality (repolabels.go — the helper
// `goobers connect`'s reality echo shares).
var (
	targetRepositoryLabels        = gitHubRepositoryLabels
	targetRepositoryWorkflowCount = gitHubActionsWorkflowCount
	validateRealityLister         = func(token string) repoWorkItemLister { return providers.NewGitHubProvider(token) }
)

// gitHubRepositoryLabels reads the repository's label names through the
// provider client (the sanctioned egress path — SEC-048 forbids hardcoded
// destinations here). Read-only by design: the seeding counterpart
// (EnsureWorkItemLabels) creates labels, which a validator must never do.
func gitHubRepositoryLabels(ctx context.Context, repo instance.RepoRef, token string) ([]string, error) {
	return providers.NewGitHubProvider(token).RepositoryLabelNames(ctx, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    repo.Owner,
		Name:     repo.Name,
	})
}

// gitHubActionsWorkflowCount reads the repository's GitHub Actions workflow
// count through the same provider client. A successful request proves the
// routed credential can read Actions metadata; its count separately detects
// repositories with no workflows.
func gitHubActionsWorkflowCount(ctx context.Context, repo instance.RepoRef, token string) (int, error) {
	return providers.NewGitHubProvider(token).ActionsWorkflowCount(ctx, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    repo.Owner,
		Name:     repo.Name,
	})
}

// checkRepositoryReality runs the --check-repos selector/CI reality pass over
// every configured repository the config actually demands something of. It
// runs after checkTargetRepositoriesAtFile has already verified each repo
// reachable, and resolves each repo's token through the same
// resolveRepoToken path. Always advisory: warnings print and land in the
// diagnostics envelope, and the return is always exit-neutral.
func checkRepositoryReality(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	stores credentials.StoreResolver,
	stdout io.Writer,
	collectors ...*diagnosticCollector,
) {
	demand := gatherRepoRealityDemand(root, configDir, cfg, set)
	if len(demand) == 0 {
		return
	}
	indexes := make([]int, 0, len(demand))
	for i := range demand {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		repo := cfg.Repos[i]
		d := demand[i]
		if d.empty() {
			continue
		}
		label := fmt.Sprintf("repos[%d] %s/%s", i, repo.Owner, repo.Name)
		if repo.Provider != "github" {
			// ADO work-item "labels" are Azure Boards tags with no cheap
			// project-wide enumeration through the provider seam, and no
			// other provider reaches this pass today — say so rather than
			// implying the config was verified (cold-start pillar: never
			// false confidence).
			pf(stdout, "REPOSITORY %s: selector/CI reality not checked for provider %q — verify selector labels/tags and CI exist manually\n",
				label, repo.Provider)
			continue
		}
		token, err := resolveRepoToken(repo, fmt.Sprintf("validate-reality-%d", i), stores)
		if err != nil {
			pf(stdout, "REPOSITORY %s: could not resolve token for selector/CI reality checks: %s\n",
				label, scrubRepositoryError(err, ""))
			continue
		}
		checkGitHubRepositoryReality(label, repo, token, d, stdout, collectors...)
	}
}

func checkGitHubRepositoryReality(
	label string,
	repo instance.RepoRef,
	token string,
	demand *repoRealityDemand,
	stdout io.Writer,
	collectors ...*diagnosticCollector,
) {
	if len(demand.labelUses) > 0 || len(demand.selectors) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		repoLabels, err := targetRepositoryLabels(ctx, repo, token)
		cancel()
		if err != nil {
			pf(stdout, "REPOSITORY %s: could not check selector labels: %s\n", label, scrubRepositoryError(err, token))
		} else {
			existing := make(map[string]bool, len(repoLabels))
			for _, name := range repoLabels {
				// GitHub label names are case-insensitive (EnsureWorkItemLabels
				// keys the same way).
				existing[strings.ToLower(name)] = true
			}
			warnMissingSelectorLabels(label, demand.labelUses, existing, stdout, collectors...)
			warnZeroEligibleItems(label, repo, token, demand.selectors, existing, stdout, collectors...)
		}
	}
	if len(demand.ciPoll) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		count, err := targetRepositoryWorkflowCount(ctx, repo, token)
		cancel()
		switch {
		case err != nil:
			cause := scrubRepositoryError(err, token)
			for _, use := range demand.ciPoll {
				message := fmt.Sprintf(
					"workflow %q stage %q polls the pull request's CI checks, but the routed credential could not read "+
						"the repository's GitHub Actions workflows (%s), so CI visibility would fail at runtime; "+
						"grant Actions: Read to the credential routed to this repository or correct its credential route",
					use.workflow, use.stage, cause)
				pf(stdout, "REPOSITORY %s: WARNING: %s\n", label, message)
				addDiagnostic(collectors, use.file, use.path, "CIPOLL001", string(validate.Warning),
					fmt.Sprintf("%s: %s", label, message))
			}
		case count == 0:
			for _, use := range demand.ciPoll {
				message := fmt.Sprintf(
					"workflow %q stage %q polls the pull request's CI checks, but the repository has no GitHub Actions workflows "+
						"(external check apps are not detected by this probe), so every run would park at the CI gate's timeout branch; "+
						"add CI to the repository or remove the ci-poll/ci-gate stages for a local-gate-only loop",
					use.workflow, use.stage)
				pf(stdout, "REPOSITORY %s: WARNING: %s\n", label, message)
				addDiagnostic(collectors, use.file, use.path, "CIPOLL001", string(validate.Warning),
					fmt.Sprintf("%s: %s", label, message))
			}
		}
	}
}

func warnMissingSelectorLabels(
	label string,
	uses []labelUse,
	existing map[string]bool,
	stdout io.Writer,
	collectors ...*diagnosticCollector,
) {
	type seenKey struct {
		label string
		where string
	}
	seen := map[seenKey]bool{}
	for _, use := range uses {
		if existing[strings.ToLower(use.label)] {
			continue
		}
		key := seenKey{label: strings.ToLower(use.label), where: use.where}
		if seen[key] {
			continue
		}
		seen[key] = true
		var code, message string
		switch use.kind {
		case labelUseApply:
			code = "SELECTOR003"
			message = fmt.Sprintf(
				"%s applies label %q, which does not exist on the repository — GitHub rejects applying labels that do not exist, "+
					"so the first run to reach that stage fails; create it (`goobers connect --seed` seeds selector labels only, "+
					"so create this one directly, e.g. `gh label create %q`)",
				use.where, use.label, use.label)
		case labelUseExclude:
			code = "SELECTOR001"
			message = fmt.Sprintf(
				"%s excludes label %q, which does not exist on the repository — the exclusion never matches anything; "+
					"check the label vocabulary for a typo or create the label",
				use.where, use.label)
		default:
			code = "SELECTOR001"
			message = fmt.Sprintf(
				"%s selects label %q, which does not exist on the repository — a selector naming it can never match, "+
					"so the loop would claim nothing (indistinguishable from an idle daemon); "+
					"create the label and apply it to real items, or fix the selector (`goobers connect --seed` can seed selector labels)",
				use.where, use.label)
		}
		pf(stdout, "REPOSITORY %s: WARNING: %s\n", label, message)
		addDiagnostic(collectors, use.file, use.path, code, string(validate.Warning),
			fmt.Sprintf("%s: %s", label, message))
	}
}

func warnZeroEligibleItems(
	label string,
	repo instance.RepoRef,
	token string,
	selectors []gaggleSelectorQuery,
	existing map[string]bool,
	stdout io.Writer,
	collectors ...*diagnosticCollector,
) {
	for _, selector := range selectors {
		missing := false
		for _, l := range selector.labels {
			if !existing[strings.ToLower(l)] {
				missing = true
				break
			}
		}
		if missing {
			// A nonexistent label already carries its own SELECTOR001 warning
			// with the claim-nothing consequence; a redundant zero-match line
			// for the same root cause would be noise.
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		reality, err := checkRepoSelectorReality(ctx, validateRealityLister(token), providers.RepositoryRef{
			Provider: providers.ProviderGitHub,
			Owner:    repo.Owner,
			Name:     repo.Name,
		}, selector.labels)
		cancel()
		if err != nil {
			pf(stdout, "REPOSITORY %s: could not check Gaggle/%s's eligible-item count: %s\n",
				label, selector.gaggle, scrubRepositoryError(err, token))
			continue
		}
		if !reality.Mismatch() {
			continue
		}
		// Summary/Remedy are repolabels.go's shared phrasing — the same
		// comparison `goobers connect` reports as its CONNECT004 note — plus
		// the workload consequence an idle-looking daemon hides.
		message := fmt.Sprintf("Gaggle/%s %s, %s — the loop is indistinguishable from an idle daemon until an item matches",
			selector.gaggle, reality.Summary(repo.Owner+"/"+repo.Name), reality.Remedy())
		pf(stdout, "REPOSITORY %s: WARNING: %s\n", label, message)
		addDiagnostic(collectors, selector.file, selector.path, "SELECTOR002", string(validate.Warning),
			fmt.Sprintf("%s: %s", label, message))
	}
}
