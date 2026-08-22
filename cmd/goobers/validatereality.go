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
// instance.yaml runner claims vs config requiredCapabilities, and gate
// completion-branch reachability — appending each finding to report (so
// --strict and the JSON report see it like any other config warning) and
// returning the findings with their file/path attribution for the
// diagnostics collector.
func appendStaticRealityWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	report *validate.Report,
) []realityWarning {
	if set == nil || report == nil {
		return nil
	}
	var warnings []realityWarning
	add := func(code validate.WarningCode, kind, name, file, path, message string) {
		report.Issues = append(report.Issues, validate.Issue{
			Code:     code,
			Severity: validate.Warning,
			Kind:     kind,
			Name:     name,
			Message:  message,
		})
		warnings = append(warnings, realityWarning{
			warning: validate.CodedWarning{
				Code:        code,
				Severity:    validate.Warning,
				Scope:       kind + "/" + name,
				Explanation: message,
			},
			file: file,
			path: path,
		})
	}
	appendUnclaimedCapabilityWarnings(root, configDir, cfg, set, add)
	appendMaxOpenPRWarnings(root, configDir, cfg, set, add)
	appendGateCompletionWarnings(root, configDir, set, add)
	return warnings
}

// appendUnclaimedCapabilityWarnings cross-checks every gaggle's whole-gaggle
// runner-capability union (gaggle spec.requiredCapabilities plus every bound
// workflow stage's requiredCapabilities — instance.RequiredCapabilities, the
// exact union the daemon's own startup check validates) against the instance
// runner's claimed set, using the scheduler's own matching primitive
// (runnercap.NewClaimed/Missing: exact string set membership, never a
// version range) so this check can never disagree with schedule-time
// matching. An unclaimed token today validates clean and then refuses every
// run: the scheduler's admit check skips the workflow each tick and
// `goobers up` fails closed at startup (RRQ-1/#1101) — the cold-start
// dotnet #7 / swift `swift@9.9` + `totally-made-up-toolchain@42` probes.
func appendUnclaimedCapabilityWarnings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	add func(code validate.WarningCode, kind, name, file, path, message string),
) {
	if cfg == nil {
		return
	}
	claimed := runnercap.NewClaimed(cfg.SelfRunnerCapabilities())
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i]
		required := instance.RequiredCapabilities(gaggle, set.Workflows)
		for _, token := range claimed.Missing(required) {
			message := fmt.Sprintf(
				"requires runner capability %q, but runner.capabilities in instance.yaml does not claim it, "+
					"so the scheduler would refuse to place every run of this gaggle and `goobers up` fails at startup; "+
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
