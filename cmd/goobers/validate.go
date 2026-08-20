package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/proc"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// copilotAuthCheckArgs is the confirmed non-interactive Copilot authentication
// probe (#284/#271). The Copilot CLI has no auth-status subcommand, and
// `--version` succeeds even when signed out, so authentication is verified by a
// minimal, tool-disabled prompt: it exits 0 when the token is valid AND has the
// "Copilot Requests" fine-grained permission, and non-zero with an actionable
// auth error otherwise. `--available-tools=` (empty allowlist) disables every
// tool so the probe can never touch the filesystem or run shell commands;
// `--allow-all-tools` is still required to enable non-interactive mode.
//
// This runs in BOTH the operator-invoked `goobers validate --check-harness` and
// the automatic daemon-startup preflight (adapterFor wires it into every
// CopilotAdapter, so preflightAgenticHarnesses picks it up too — #238). It costs
// a real Copilot request (~a few AI credits, a couple of seconds), but
// preflightAgenticHarnesses runs once per process lifetime (once per `up` daemon
// boot, once per `run`), only for harnesses an agentic stage actually
// references — trivial next to the ~30-minute burned live-run a signed-out
// harness causes when the failure surfaces mid-run instead (the #284 incident).
var copilotAuthCheckArgs = []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}

// harnessPreflightTimeout bounds a single harness preflight (its version check
// plus the auth probe's real API round-trip) so a hung CLI or network can't
// hang `goobers validate` or `goobers up`/`run` startup.
const harnessPreflightTimeout = 90 * time.Second

const placeholderFindingCode = "PLACEHOLDER001"

var templateMarkers = []string{"your-org", "your-repo"}

const validateHelp = "Usage: goobers validate [--json] [--github-annotations] [--check-harness] [--check-repos] [--source-tree] [--strict] [path]\n\n" +
	"Validate an instance's instance.yaml and config/ directory (default\n" +
	"path \".\"). --source-tree validates a checked-in config source tree\n" +
	"using instance.yaml.example and the path itself as config/. " +
	"--strict treats config warnings as validation errors. " +
	"--json emits a versioned findings envelope instead of human-readable output. " +
	"--github-annotations additionally writes each finding to stderr as a\n" +
	"GitHub Actions ::error/::warning file annotation (#687), so a\n" +
	"config-repo PR check surfaces failures directly on the PR diff; " +
	"composes with --json since stdout stays untouched. " +
	"--check-harness additionally preflights every agent harness\n" +
	"referenced by a goober (GBO-011) — installed, signed in, actionable\n" +
	"guidance otherwise. --check-repos resolves each target repository's\n" +
	"token, verifies authenticated git access, and (GitHub only) warns when\n" +
	"a repository is larger than the checkout-size threshold. Exit codes:\n" +
	"0 = valid, 1 = validation errors, 2 = usage/IO error.\n"

func runValidate(args []string, stdout, stderr io.Writer) int {
	return runValidateAs("validate", args, stdout, stderr)
}

func runStartupConfigPreflight(root string, skip bool, stderr io.Writer) int {
	var output bytes.Buffer
	// Startup runs the same single validation engine, in its non-spawning
	// discovery mode — see validateOptions.deferModelDiscovery (#3336).
	code := runValidateAsDeferring("validate", []string{root}, &output, &output, true)
	if skip {
		if code == 0 {
			pf(stderr, "WARNING: --skip-preflight enabled; startup validation enforcement is disabled\n")
		} else {
			pf(stderr, "WARNING: --skip-preflight enabled; ignoring the following startup validation errors:\n")
			_, _ = output.WriteTo(stderr)
		}
		return 0
	}
	if code != 0 {
		_, _ = output.WriteTo(stderr)
	}
	return code
}

// runValidateAs is the single authoritative config-validation engine shared by
// `goobers validate` and its `goobers lint` alias (#439/AUTH-2). name selects
// which verb's flag-parse label and registered `-h` help surface are used so
// each command keeps its own identity, but both run byte-for-byte the same
// checks and exit codes — there is exactly one validation path, never a weaker
// second one that could disagree (the #252 footgun this closes for good).
func runValidateAs(name string, args []string, stdout, stderr io.Writer) int {
	return runValidateAsDeferring(name, args, stdout, stderr, false)
}

// runValidateAsDeferring is runValidateAs with the validation engine's one
// internal, non-CLI knob: deferModelDiscovery — the daemon's startup preflight
// mode (#3336). Model discovery spawns the Copilot CLI, and in a memory-capped
// pod the spawned children can OOM the daemon before the discovery timeout
// fires — so the startup pass accepts models unverified (the same degradation
// as an unreachable CLI) instead of spawning. Interactive `goobers validate`
// keeps discovery live; this is a survival knob for the in-process caller, not
// a second, weaker validation path (#252's single-engine rule still holds —
// same engine, one documented divergence).
func runValidateAsDeferring(name string, args []string, stdout, stderr io.Writer, deferModelDiscovery bool) int {
	fs := newCLIFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit a versioned machine-readable findings envelope")
	githubAnnotations := fs.Bool("github-annotations", false, "also write each finding to stderr as a GitHub Actions file annotation (#687)")
	checkHarness := fs.Bool("check-harness", false, "also verify every referenced agent harness is installed and signed in")
	checkRepos := fs.Bool("check-repos", false, "also verify every target repository is reachable with its configured credential")
	sourceTree := fs.Bool("source-tree", false, "validate a checked-in config tree containing instance.yaml.example, manifest.yaml, and gaggles/")
	strict := fs.Bool("strict", false, "treat config warnings as validation errors")
	fs.Usage = helpUsage(stderr, name)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	var diagnostics *diagnosticCollector
	humanOut, humanErr := stdout, stderr
	if *asJSON {
		diagnostics = &diagnosticCollector{}
		humanOut = io.Discard
		humanErr = io.Discard
	} else if *githubAnnotations {
		diagnostics = &diagnosticCollector{}
	}
	code := runValidateConfig(validateOptions{
		root:                root,
		sourceTree:          *sourceTree,
		checkHarness:        *checkHarness,
		checkRepos:          *checkRepos,
		strict:              *strict,
		deferModelDiscovery: deferModelDiscovery,
	}, humanOut, humanErr, diagnostics)
	if *githubAnnotations {
		emitGitHubAnnotations(stderr, diagnostics)
	}
	if !*asJSON {
		return code
	}
	if err := encodeSchemaJSON(stdout, schemas.Diagnostics, diagnostics.envelope(code == 0)); err != nil {
		pf(stderr, "error: encode diagnostics: %v\n", err)
		return 1
	}
	return code
}

type validateOptions struct {
	root         string
	sourceTree   bool
	checkHarness bool
	checkRepos   bool
	strict       bool
	// deferModelDiscovery is set only by the daemon's startup preflight —
	// see runValidateAsDeferring (#3336). Never set from a CLI flag.
	deferModelDiscovery bool
}

func runValidateConfig(options validateOptions, stdout, stderr io.Writer, diagnostics *diagnosticCollector) int {
	root := options.root
	l := instance.NewLayout(root)
	configFile := l.ConfigFile()
	configDir := l.ConfigDir()
	if options.sourceTree {
		configFile = filepath.Join(root, "instance.yaml.example")
		configDir = root
	}
	if _, err := os.Stat(configFile); err != nil {
		if options.sourceTree {
			pf(stderr, "error: %s not found (not a config source tree)\n", configFile)
			diagnostics.add(diagnosticFile(root, configFile), "/", "IO001", string(validate.Error),
				fmt.Sprintf("%s not found (not a config source tree)", configFile))
		} else {
			pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", configFile)
			diagnostics.add(diagnosticFile(root, configFile), "/", "IO001", string(validate.Error),
				fmt.Sprintf("%s not found (not an instance root — run `goobers init` first)", configFile))
		}
		return 2
	}

	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		pf(stdout, "INVALID instance.yaml:\n  %v\n", err)
		diagnostics.add(diagnosticFile(root, configFile), "/", "INSTANCE001", string(validate.Error), err.Error())
		return 1
	}

	set, report, err := loadConfigDirectory(configDir)
	if err != nil && !errors.Is(err, instance.ErrInvalidConfig) {
		pf(stderr, "error: %v\n", err)
		diagnostics.add(diagnosticFile(root, configDir), "/", "IO001", string(validate.Error), err.Error())
		return 2
	}
	diagnostics.addReport(report, diagnosticFile(root, configDir))
	printValidationIssues(stdout, report)
	if errors.Is(err, instance.ErrInvalidConfig) {
		// The legacy single-repo fallback can only be observed after decoding
		// the schema-invalid empty project. Preserve the schema errors while
		// still telling operators which repository runtime would bind.
		comparisonSet, comparisonReport, comparisonErr := instance.LoadConfigDirForComparison(configDir)
		if comparisonSet != nil && comparisonReport != nil && comparisonReport.HasErrors() &&
			errors.Is(comparisonErr, instance.ErrInvalidConfig) {
			_ = checkGaggleRepositoryBindings(root, configDir, cfg, comparisonSet, stdout, diagnostics)
		}
		pf(stdout, "\nconfig directory failed validation\n")
		return 1
	}
	placeholderFindings, err := findTemplatePlaceholders(root, configFile, configDir)
	if err != nil {
		pf(stderr, "error: inspect configuration placeholders: %v\n", err)
		diagnostics.add(diagnosticFile(root, configDir), "/", "IO001", string(validate.Error), err.Error())
		return 2
	}
	placeholderSeverity := validate.Warning
	if options.strict {
		placeholderSeverity = validate.Error
	}
	for _, finding := range placeholderFindings {
		pf(stdout, "%s %s %s: %s\n",
			strings.ToUpper(string(placeholderSeverity)), placeholderFindingCode, finding.file, finding.message)
		diagnostics.add(finding.file, "/", placeholderFindingCode, string(placeholderSeverity), finding.message)
	}

	// api/validate's cross-reference checks (above) mirror most of
	// workflow.Compile's own semantic analysis (CheckReachability/
	// CheckSchedules/CheckGateOutcomes/CheckWorkflowAdmission), but this is the one
	// point that actually calls Compile with the same options `up`/`run` use
	// at daemon startup — including WithKnownChecks, which nothing else here
	// validates (#124). A config that fails this would also fail to start
	// the daemon; catching that now, at `validate` time, is the whole point.
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		pf(stdout, "\nINVALID workflow: %v\n", err)
		file := diagnosticFile(root, configDir)
		var instructionsErr *gooberInstructionsError
		if errors.As(err, &instructionsErr) {
			file = gooberDiagnosticFile(root, configDir, set, instructionsErr.Goober)
		}
		diagnostics.add(file, "/spec/instructions", "GBO004", string(validate.Error), err.Error())
		return 1
	}
	_, _, _, harnessWarnings, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand,
		options.deferModelDiscovery,
	)
	if err != nil {
		pf(stdout, "\nINVALID workflow: %v\n", err)
		file, path, code := compiledConfigDiagnostic(root, configDir, set, err)
		diagnostics.add(file, path, code, string(validate.Error), err.Error())
		return 1
	}
	codedWarnings, err := appendGooberHarnessWarnings(report, harnessWarnings)
	if err != nil {
		pf(stderr, "error: append harness validation warnings: %v\n", err)
		return 2
	}
	for _, warning := range codedWarnings {
		diagnostics.add(
			gooberDiagnosticFile(root, configDir, set, strings.TrimPrefix(warning.Scope, "Goober/")),
			"/spec/harnessOptions/fallback-to-default",
			string(warning.Code),
			string(warning.Severity),
			warning.Explanation,
		)
	}
	printValidationWarnings(stdout, codedWarnings)
	skillWarnings, err := appendSkillPackageCollisionWarnings(configDir, report, goobers)
	if err != nil {
		pf(stderr, "error: inspect skill package collisions: %v\n", err)
		return 2
	}
	for _, warning := range skillWarnings {
		diagnostics.add(
			diagnosticFile(root, filepath.Join(configDir, "gaggles", strings.TrimPrefix(warning.Scope, "Gaggle/"), "skills")),
			"/",
			string(warning.Code),
			string(warning.Severity),
			warning.Explanation,
		)
	}
	printValidationWarnings(stdout, skillWarnings)

	// Static reality cross-checks (2026-08-08 cold-start audit): a
	// requiredCapabilities token no runner claims (CAP003), an unenforceable
	// maxOpenPRs cap (PRCAP001), and an automated gate completion branch a
	// failed stage can never complete through (WF018). Appended to the report
	// like the harness/skill warnings above, so --strict and the JSON report
	// treat them as ordinary config warnings.
	staticRealityWarnings := appendStaticRealityWarnings(root, configDir, cfg, set, report)
	for _, finding := range staticRealityWarnings {
		diagnostics.add(finding.file, finding.path, string(finding.warning.Code),
			string(finding.warning.Severity), finding.warning.Explanation)
		pln(stdout, finding.warning.String())
	}

	// Docs-location existence (#1016). The config-load pass (api/validate) has
	// already rejected empty/absolute/escaping docs roots lexically; this adds
	// the filesystem half — a declared root that does not exist in the
	// repository — which api-level validation cannot do because it has no repo
	// tree. docsRoots name paths in each gaggle's TARGET repository
	// (spec.project), so the existence check is authoritative only when the git
	// tree containing the config IS that repository; any other tree — a
	// standalone workflowSource repo, a non-git directory — gets an advisory
	// warning instead of an error or a silent skip (#3285).
	if !checkDocsRootsExist(root, configDir, cfg, set, stdout, diagnostics) {
		return 1
	}

	// #650: a shipped config's deterministic stages invoke the goobers CLI by
	// name (Task.Run.Command, e.g. `goobers backlog-query --claim`). A verb that
	// no longer exists — renamed, removed, or a typo — would compile clean here
	// and only fail once the runner shells out to it mid-run. Cross-check every
	// such command against the CLI registry now, at validate time, so config
	// drift from the CLI surface is caught before it reaches a live run.
	if problems := stageCommandProblems(set); len(problems) > 0 {
		for _, problem := range problems {
			pln(stdout, problem.message)
			diagnostics.add(configSourceDiagnosticFile(root, configDir, problem.source), problem.path,
				"COMMAND001", string(validate.Error), problem.message)
		}
		pf(stdout, "\nconfig references CLI stage commands that do not exist\n")
		return 1
	}

	// CONF-6/#2079: a workflow's provider-capability requirements (declared or
	// derived from the stages it uses) must be satisfiable by its gaggle's
	// connected provider. Checked at validate time for the same reason as
	// CheckCapabilityRequirements's daemon-startup check: an unmet
	// requirement can never self-heal at runtime, so it should fail here
	// rather than at the first mid-run ErrUnsupported.
	if err := instance.CheckProviderCapabilityRequirements(set); err != nil {
		pf(stdout, "\nINVALID workflow: %v\n", err)
		diagnostics.add(diagnosticFile(root, configDir), "/spec/requires/capabilities", "PROV001", string(validate.Error), err.Error())
		return 1
	}

	if !checkGaggleRepositoryBindings(root, configDir, cfg, set, stdout, diagnostics) {
		pf(stdout, "\ngaggle repositories do not match instance repos[]\n")
		return 1
	}

	if options.checkHarness {
		harnessStores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			pf(stdout, "INVALID secretStores:\n  %v\n", err)
			diagnostics.add(diagnosticFile(root, configFile), "/secretStores", "INSTANCE002", string(validate.Error), err.Error())
			return 1
		}
		modelCredential, err := agentModelCredentialResolver(cfg, harnessStores)
		if err != nil {
			pf(stdout, "INVALID credentials:\n  %v\n", err)
			diagnostics.add(diagnosticFile(root, configFile), "/credentials", "INSTANCE003", string(validate.Error), err.Error())
			return 1
		}
		if !checkHarnessesAtSources(set.Goobers, stdout, stderr, func(goober apiv1.Goober) string {
			return gooberDiagnosticFile(root, configDir, set, goober.Name)
		}, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, modelCredential, diagnostics) {
			return 1
		}
	}
	if options.checkRepos {
		stores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			pf(stdout, "INVALID secretStores:\n  %v\n", err)
			diagnostics.add(diagnosticFile(root, configFile), "/secretStores", "INSTANCE002", string(validate.Error), err.Error())
			return 1
		}
		if !checkTargetRepositoriesAtFile(cfg.Repos, stores, stdout, diagnosticFile(root, configFile), diagnostics) {
			return 1
		}
		// Selector/CI reality (2026-08-08 cold-start audit, README item 1):
		// --check-repos just contacted every repository, so additionally
		// compare each repo's actual label set, live eligible-item count, and
		// CI-workflow presence against what the config demands of it.
		// Advisory only — repo state is not config, so these warnings never
		// change the exit code (same contract as the #1547 size warning).
		checkRepositoryReality(root, configDir, cfg, set, stores, stdout, diagnostics)
	}
	printDSLVersionSummary(stdout, set.Workflows)
	// Deprecation notices (DVL020) are strict-neutral by ruling: a deprecated
	// dslVersion stays fully supported, so nudging users to migrate must never
	// turn an existing green pipeline red. They print and land in diagnostics
	// but are excluded from --strict's promotion.
	deprecationCount := 0
	for _, w := range report.Warnings() {
		if w.Code == validate.WarningDeprecatedDSLVersion {
			deprecationCount++
		}
	}
	warningCount := len(report.Warnings()) - deprecationCount + len(placeholderFindings)
	if options.strict && warningCount > 0 {
		pf(stdout, "\nconfiguration has %d warning(s); --strict treats warnings as errors\n", warningCount)
		return 1
	}
	printResolvedLargeRepoPresets(stdout, cfg.Repos)
	pf(stdout, "OK: instance.yaml valid; config/ valid (%d gaggle(s), %d goober(s), %d workflow(s))\n",
		len(set.Gaggles), len(set.Goobers), len(set.Workflows))
	return 0
}

type placeholderFinding struct {
	file    string
	message string
}

func findTemplatePlaceholders(root, configFile, configDir string) ([]placeholderFinding, error) {
	paths := []string{configFile}
	seenPaths := map[string]bool{filepath.Clean(configFile): true}
	err := filepath.WalkDir(configDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
			clean := filepath.Clean(path)
			if !seenPaths[clean] {
				seenPaths[clean] = true
				paths = append(paths, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	var findings []placeholderFinding
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var found []string
		for _, marker := range templateMarkers {
			if containsTemplateMarker(data, marker) {
				found = append(found, marker)
			}
		}
		if len(found) == 0 {
			continue
		}
		findings = append(findings, placeholderFinding{
			file: diagnosticFile(root, path),
			message: fmt.Sprintf(
				"contains unedited template marker(s) %s; replace them with the target repository coordinates",
				strings.Join(found, ", "),
			),
		})
	}
	return findings, nil
}

func containsTemplateMarker(data []byte, marker string) bool {
	target := []byte(marker)
	for offset := 0; offset < len(data); {
		index := bytes.Index(data[offset:], target)
		if index < 0 {
			return false
		}
		index += offset
		beforeBoundary := index == 0 || !isRepositoryCoordinateByte(data[index-1])
		after := index + len(target)
		afterBoundary := after == len(data) || !isRepositoryCoordinateByte(data[after])
		if beforeBoundary && afterBoundary {
			return true
		}
		offset = index + len(target)
	}
	return false
}

func isRepositoryCoordinateByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '-' || value == '_' || value == '.'
}

func printResolvedLargeRepoPresets(out io.Writer, repos []instance.RepoRef) {
	for _, repo := range repos {
		if !repo.LargeRepo {
			continue
		}
		workspace := "worktrees"
		mirrorRefspec := "all"
		if repo.Pinned() {
			workspace = "pinned"
			mirrorRefspec = "heads+tags"
		}
		pathLength := "disabled"
		if repo.PathLength != nil && !repo.PathLength.Disabled {
			maximum := repo.PathLength.MaxPathLength
			if maximum == 0 {
				maximum = worktree.DefaultMaxPathLength
			}
			pathLength = fmt.Sprintf("enabled (max %d)", maximum)
		}
		pf(out, "Resolved large-repo preset for %s/%s: workspace=%s, cleanPolicy=%s, serial=%t, defaultStageTimeout=%s, stalledRunTimeout=%s, maxRunDuration=%s, pathLength=%s, mirrorRefspec=%s\n",
			repo.Owner, repo.Name, workspace, repo.WorkspaceCleanPolicy(), repo.Pinned(),
			repo.DefaultStageTimeout, repo.RunControls.StalledRunTimeout, repo.RunControls.MaxRunDuration, pathLength, mirrorRefspec)
	}
}

func configSourceDiagnosticFile(root, configDir, source string) string {
	if source == "" {
		return diagnosticFile(root, configDir)
	}
	return diagnosticFile(root, filepath.Join(configDir, filepath.FromSlash(source)))
}

func gooberDiagnosticFile(root, configDir string, set *instance.ConfigSet, name string) string {
	source, _ := set.GooberSource(name)
	return configSourceDiagnosticFile(root, configDir, source)
}

func gaggleDiagnosticFile(root, configDir string, set *instance.ConfigSet, name string) string {
	source, _ := set.GaggleSource(name)
	return configSourceDiagnosticFile(root, configDir, source)
}

func checkGaggleRepositoryBindings(
	root, configDir string,
	cfg *instance.Config,
	set *instance.ConfigSet,
	stdout io.Writer,
	diagnostics *diagnosticCollector,
) bool {
	ok := true
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		if gaggleUsesOnlyScratchWorkspaces(set, gaggle.Name) {
			// Scratch-only workflows never perform the runtime repository join,
			// regardless of how many repos[] entries the instance configures
			// (possibly for other gaggles): the cross-check below would
			// otherwise misfire on a gaggle that never touches repos[] at all.
		} else if len(cfg.Repos) == 1 && project.Owner == "" && project.Name == "" {
			repo := cfg.Repos[0]
			message := fmt.Sprintf("empty spec.project binds to instance repos[0] %s", instanceRepoName(repo))
			pf(stdout, "INFO Gaggle/%s: %s\n", gaggle.Name, message)
			diagnostics.add(gaggleDiagnosticFile(root, configDir, set, gaggle.Name),
				"/spec/project", "REPO003", diagnosticSeverityInfo, message)
		} else if _, found := configuredRepoForProject(cfg, project); !found {
			message := unmatchedGaggleRepoMessage("spec.project", project, cfg.Repos)
			pf(stdout, "ERROR Gaggle/%s: %s\n", gaggle.Name, message)
			diagnostics.add(gaggleDiagnosticFile(root, configDir, set, gaggle.Name),
				"/spec/project", "REPO002", string(validate.Error), message)
			ok = false
		}
		for i, repo := range gaggle.Spec.AdditionalRepos {
			if _, found := configuredRepoForProject(cfg, repo); found {
				continue
			}
			field := fmt.Sprintf("spec.additionalRepos[%d]", i)
			message := unmatchedGaggleRepoMessage(field, repo, cfg.Repos)
			pf(stdout, "ERROR Gaggle/%s: %s\n", gaggle.Name, message)
			diagnostics.add(gaggleDiagnosticFile(root, configDir, set, gaggle.Name),
				fmt.Sprintf("/spec/additionalRepos/%d", i), "REPO002", string(validate.Error), message)
			ok = false
		}
	}
	return ok
}

func gaggleUsesOnlyScratchWorkspaces(set *instance.ConfigSet, gaggle string) bool {
	found := false
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle != gaggle {
			continue
		}
		found = true
		for _, task := range workflow.Spec.Tasks {
			if task.Type == apiv1.TaskAgentic || task.Run == nil || task.Run.Workspace != apiv1.WorkspaceScratch {
				return false
			}
		}
		for _, gate := range workflow.Spec.Gates {
			if gate.Evaluator == apiv1.EvaluatorAgentic {
				return false
			}
		}
	}
	return found
}

func unmatchedGaggleRepoMessage(field string, repo apiv1.RepoRef, configured []instance.RepoRef) string {
	message := fmt.Sprintf("%s repository %s matches no instance repos[] entry", field, apiRepoName(repo))
	if suggestion, found := suggestConfiguredRepo(repo, configured); found {
		message += fmt.Sprintf("; did you mean %q?", instanceRepoName(suggestion))
	}
	return message
}

func suggestConfiguredRepo(repo apiv1.RepoRef, configured []instance.RepoRef) (instance.RepoRef, bool) {
	wanted := strings.ToLower(repo.Owner + "/" + repo.Name)
	bestDistance := -1
	var best instance.RepoRef
	for _, candidate := range configured {
		if repo.Provider != "" && candidate.Provider != string(repo.Provider) {
			continue
		}
		distance := repositoryEditDistance(wanted, strings.ToLower(candidate.Owner+"/"+candidate.Name))
		if bestDistance == -1 || distance < bestDistance {
			bestDistance = distance
			best = candidate
		}
	}
	if bestDistance < 0 || bestDistance > 3 {
		return instance.RepoRef{}, false
	}
	return best, true
}

func repositoryEditDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for i := range previous {
		previous[i] = i
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

func apiRepoName(repo apiv1.RepoRef) string {
	if repo.Project != "" {
		return fmt.Sprintf("%s/%s/%s", repo.Owner, repo.Project, repo.Name)
	}
	return repo.Owner + "/" + repo.Name
}

func instanceRepoName(repo instance.RepoRef) string {
	if repo.Project != "" {
		return fmt.Sprintf("%s/%s/%s", repo.Owner, repo.Project, repo.Name)
	}
	return repo.Owner + "/" + repo.Name
}

func compiledConfigDiagnostic(root, configDir string, set *instance.ConfigSet, err error) (file, path, code string) {
	var harnessErr *gooberHarnessConfigError
	if errors.As(err, &harnessErr) {
		return gooberDiagnosticFile(root, configDir, set, harnessErr.Goober), "/spec/harness", "HARNESS002"
	}
	var compileErr *workflowCompileError
	if errors.As(err, &compileErr) {
		source, _ := set.WorkflowSource(compileErr.Gaggle, compileErr.Workflow)
		return configSourceDiagnosticFile(root, configDir, source), "/", "COMPILE001"
	}
	var digestErr *workflowDigestError
	if errors.As(err, &digestErr) {
		source, _ := set.WorkflowSource(digestErr.Gaggle, digestErr.Workflow)
		return configSourceDiagnosticFile(root, configDir, source), "/", "COMPILE002"
	}
	return diagnosticFile(root, configDir), "/", "INTERNAL001"
}

// checkDocsRootsExist verifies every workflow-declared docs root exists in the
// gaggle's target repository (#1016). base is the user-supplied validate path
// (a config source tree or an instance root). docsRoots name paths in the
// repository the gaggle works on (spec.project), NOT in the config tree, so
// the existence check is authoritative only when the git working tree
// containing base is a checkout of that repository — decided by matching the
// target's owner/name against the tree's git remotes (#3285). There it
// returns false (failing validation) when a declared root is missing, exactly
// as before. Any other tree — a standalone workflowSource repo with a
// different remote, a tree with no remotes, or no git repository at all
// (which pre-#3285 skipped SILENTLY) — proves nothing about the target repo,
// so each declared root is reported as an advisory DOCS003 warning naming the
// repository the roots will be checked against at runtime. Advisory means
// exit-neutral and excluded from --strict's promotion, the same contract as
// checkRepositoryReality's findings: where validate happens to run is machine
// state, not config, and must never turn a green tree red.
func checkDocsRootsExist(base, configDir string, cfg *instance.Config, set *instance.ConfigSet, stdout io.Writer, collectors ...*diagnosticCollector) bool {
	type declaredRoot struct {
		workflow string
		gaggle   string
		source   string
		root     string
		index    int
	}
	var declared []declaredRoot
	for _, w := range set.Workflows {
		source, _ := set.WorkflowSource(w.Spec.Gaggle, w.Name)
		for i, dr := range w.Spec.DocsRoots {
			declared = append(declared, declaredRoot{workflow: w.Name, gaggle: w.Spec.Gaggle, source: source, root: dr, index: i})
		}
	}
	if len(declared) == 0 {
		return true
	}
	repoRoot, toplevelErr := gitToplevel(base)
	var treeRemotes []string
	if toplevelErr == nil {
		treeRemotes = gitRemoteURLs(repoRoot)
	}
	ok := true
	for _, d := range declared {
		owner, name := docsRootTargetRepository(cfg, set, d.gaggle)
		if toplevelErr != nil {
			// Not inside a git repository at all — the permanent, expected
			// state of every INSTANCE ROOT (an instance root is never a
			// checkout of anything). A WARNING here would fire on every
			// `goobers init` and every instance-root validate forever, with
			// nothing the operator can do about it — unactionable noise that
			// trains people to ignore warnings. Print an informational line
			// (never a silent skip, #3285) and move on; DOCS003 stays
			// reserved for the actionable case below.
			pf(stdout, "DOCSROOTS Workflow/%s: declared docs root %q not verified here (checked at runtime against %s/%s)\n",
				d.workflow, d.root, owner, name)
			continue
		}
		if !remotesNameRepository(treeRemotes, owner, name) {
			// A git checkout whose remotes do not name the target repository:
			// a standalone workflowSource repo, or a checkout of the WRONG
			// repo. Actionable (the operator can verify against the real
			// target), so it earns the advisory diagnostic gnyaml-style
			// allowlists track (#3285).
			message := fmt.Sprintf("declared docs root %q not verified: config tree is not the target repository %s/%s",
				d.root, owner, name)
			pf(stdout, "WARNING DOCS003 Workflow/%s: %s\n", d.workflow, message)
			addDiagnostic(collectors, configSourceDiagnosticFile(base, configDir, d.source),
				fmt.Sprintf("/spec/docsRoots/%d", d.index), "DOCS003", string(validate.Warning), message)
			continue
		}
		clean := filepath.Clean(strings.TrimSpace(d.root))
		full := filepath.Join(repoRoot, clean)
		if _, statErr := os.Stat(full); statErr != nil {
			pf(stdout, "DOCSROOTS Workflow/%s: declared docs root %q does not exist in the repository (%s)\n",
				d.workflow, d.root, repoRoot)
			addDiagnostic(collectors, configSourceDiagnosticFile(base, configDir, d.source),
				fmt.Sprintf("/spec/docsRoots/%d", d.index), "DOCS002", string(validate.Error),
				fmt.Sprintf("declared docs root %q does not exist in the repository (%s)", d.root, repoRoot))
			ok = false
		}
	}
	return ok
}

// docsRootTargetRepository resolves the repository a gaggle's docsRoots refer
// to: its spec.project, or — mirroring the runtime single-repo binding that
// checkGaggleRepositoryBindings reports as REPO003 — instance repos[0] when
// the project is empty and exactly one repository is configured.
func docsRootTargetRepository(cfg *instance.Config, set *instance.ConfigSet, gaggleName string) (owner, name string) {
	for _, gaggle := range set.Gaggles {
		if gaggle.Name == gaggleName {
			owner, name = gaggle.Spec.Project.Owner, gaggle.Spec.Project.Name
			break
		}
	}
	if owner == "" && name == "" && cfg != nil && len(cfg.Repos) == 1 {
		owner, name = cfg.Repos[0].Owner, cfg.Repos[0].Name
	}
	return owner, name
}

// remotesNameRepository reports whether any configured git remote plausibly
// names the owner/name repository — the #3285 test for "the validated tree IS
// the gaggle's target repo", deliberately network-free.
func remotesNameRepository(remotes []string, owner, name string) bool {
	if owner == "" || name == "" {
		return false
	}
	for _, remote := range remotes {
		if remoteURLNamesRepository(remote, owner, name) {
			return true
		}
	}
	return false
}

// remoteURLNamesRepository matches one remote URL against owner/name: the
// last path segment (sans .git) must be the repository name and the owner
// must appear as an earlier path segment. Segment matching tolerates the URL
// shapes in the wild — https://host/owner/name, ssh://git@host/owner/name,
// scp-like git@host:owner/name, and ADO's org/project/_git/repo — without a
// per-provider parser; a local mirror path that carries neither coordinate
// simply fails to match and downgrades to the advisory warning.
func remoteURLNamesRepository(remote, owner, name string) bool {
	path := remote
	if scheme := strings.Index(path, "://"); scheme >= 0 {
		path = path[scheme+len("://"):]
		slash := strings.Index(path, "/")
		if slash < 0 {
			return false
		}
		path = path[slash+1:]
	} else if colon := strings.Index(path, ":"); colon >= 0 && !strings.Contains(path[:colon], "/") {
		path = path[colon+1:]
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 {
		return false
	}
	if !strings.EqualFold(strings.TrimSuffix(segments[len(segments)-1], ".git"), name) {
		return false
	}
	for _, segment := range segments[:len(segments)-1] {
		if strings.EqualFold(segment, owner) {
			return true
		}
	}
	return false
}

func gitToplevel(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRemoteURLs returns every remote URL configured for the repository at
// repoRoot, in config order; nil when there are none. Reading raw config
// values (not `git remote get-url`, which applies insteadOf rewrites) keeps
// the #3285 target-repo match on what the operator actually declared.
func gitRemoteURLs(repoRoot string) []string {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--get-regexp", `^remote\..*\.url$`)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if _, url, found := strings.Cut(strings.TrimSpace(line), " "); found && url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

type stageCommandFinding struct {
	source  string
	path    string
	message string
}

// stageCommandProblems reports every deterministic stage whose `goobers …`
// Command references a CLI verb — or, for a command group, a subcommand — that
// the command registry does not define (#650). It is the compile-time guardrail
// against a shipped config drifting from the actual CLI surface. Non-goobers
// commands (arbitrary shell) are out of scope; their existence is not something
// this binary can vouch for.
func stageCommandProblems(set *instance.ConfigSet) []stageCommandFinding {
	var problems []stageCommandFinding
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		source, _ := set.WorkflowSource(wf.Spec.Gaggle, wf.Name)
		for taskIndex, task := range wf.Spec.Tasks {
			if task.Type != apiv1.TaskDeterministic || task.Run == nil {
				continue
			}
			// Only a shell stage actually executes its run.command. A stage that
			// declares a built-in kind (e.g. kind=ci-poll) is dispatched to a
			// dedicated executor and its command is an inert placeholder — the
			// shipped ci-poll stages carry `command: ["goobers", "ci-poll", …]`,
			// which is not a CLI verb — so it must not be surface-checked here.
			if kind := strings.TrimSpace(task.Inputs[executor.InputKind]); kind != "" && kind != executor.KindShell {
				continue
			}
			for _, message := range stageCommandProblem(wf.Name, task.Name, task.Run.Command) {
				problems = append(problems, stageCommandFinding{
					source:  source,
					path:    fmt.Sprintf("/spec/tasks/%d/run/command", taskIndex),
					message: message,
				})
			}
		}
	}
	return problems
}

// stageCommandProblem resolves one `goobers …` stage command against the
// registry. It reports an unknown top-level verb, and — for a command group,
// which has no runnable action of its own — a missing or unknown subcommand.
// It deliberately does not treat a non-subcommand token after a runnable verb
// as an error: commands like `run <workflow>` take positional arguments that
// must not be mistaken for subcommands.
func stageCommandProblem(workflow, task string, argv []string) []string {
	if len(argv) == 0 || argv[0] != "goobers" {
		return nil
	}
	if len(argv) < 2 {
		return []string{fmt.Sprintf(
			"workflow %q task %q: stage command %q names no goobers subcommand to run",
			workflow, task, strings.Join(argv, " "))}
	}
	verb := argv[1]
	command, ok := findCLICommand(verb)
	if !ok {
		return []string{fmt.Sprintf(
			"workflow %q task %q: stage command references unknown goobers verb %q (see `goobers help`)",
			workflow, task, verb)}
	}
	// A command group (no action of its own) is not runnable without a
	// subcommand, so the next token must name a real one.
	if !command.actionRegistered && len(command.subcommands) > 0 {
		if len(argv) < 3 || strings.HasPrefix(argv[2], "-") {
			return []string{fmt.Sprintf(
				"workflow %q task %q: stage command %q needs a subcommand for the %q command group",
				workflow, task, strings.Join(argv, " "), verb)}
		}
		if _, ok := findCLICommandIn(command.subcommands, argv[2]); !ok {
			return []string{fmt.Sprintf(
				"workflow %q task %q: stage command references unknown %q subcommand %q",
				workflow, task, verb, argv[2])}
		}
	}
	return nil
}

const (
	repositoryPreflightTimeout = 30 * time.Second
	repositoryKillWaitDelay    = time.Second
)

// oversizedRepoThresholdKB is the target-repo size (GitHub's repo API "size"
// field, in KB) above which `--check-repos` warns at validate time (#1547).
// 1 GiB is large enough that a full clone/checkout of the repo measurably
// slows down provisioning; sparse/partial checkout (AdditionalRepos, or
// project.checkout.sparse, #649) is the recommended remediation.
const oversizedRepoThresholdKB = 1 << 20

var targetRepositoryReachable = gitRepositoryReachable

// printDSLVersionSummary renders each workflow's dslVersion pin and its
// lifecycle level against this binary's supportmatrix.SupportMatrix (DVL-3,
// #863) — the line that makes version drift visible at a glance. Every level
// prints (including the common "supported" case) so an author can see the
// full picture in one place; api/validate's checkWorkflowDSLVersion is what
// actually blocks/warns on a problematic pin — this is a summary, not a
// second enforcement path.
func printDSLVersionSummary(stdout io.Writer, workflows []apiv1.Workflow) {
	matrix := supportmatrix.GetDSL()
	for _, w := range workflows {
		version := w.DSLVersion
		defaulted := ""
		if version == "" {
			version = supportmatrix.CurrentDSLVersion
			defaulted = " (defaulted; no dslVersion pin)"
		}
		support, ok := matrix.Lookup(version)
		if !ok {
			pf(stdout, "DSLVERSION Workflow/%s: %s%s, unrecognized by this binary\n", w.Name, version, defaulted)
			continue
		}
		detail := string(support.Level)
		if support.Replacement != "" {
			detail += fmt.Sprintf(", replacement %s", support.Replacement)
		}
		if support.UnsupportedAfter != "" {
			detail += fmt.Sprintf(", unsupported after %s", support.UnsupportedAfter)
		}
		pf(stdout, "DSLVERSION Workflow/%s: %s%s (%s)\n", w.Name, version, defaulted, detail)
	}
}

// targetRepositorySize resolves a GitHub repo's size in KB for the
// oversized-repo warning (#1547). Overridable in tests. ADO has no
// equivalent check yet — callers only invoke this for repo.Provider ==
// "github".
var targetRepositorySize = gitHubRepositorySize

func gitHubRepositorySize(ctx context.Context, repo instance.RepoRef, token string) (int64, error) {
	return providers.NewGitHubProvider(token).RepositorySizeKB(ctx, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    repo.Owner,
		Name:     repo.Name,
	})
}

func checkTargetRepositoriesAtFile(
	repos []instance.RepoRef,
	stores credentials.StoreResolver,
	stdout io.Writer,
	file string,
	collectors ...*diagnosticCollector,
) bool {
	if len(repos) == 0 {
		pln(stdout, "REPOSITORY: no target repositories configured; nothing to check")
		return true
	}
	ok := true
	for i, repo := range repos {
		label := fmt.Sprintf("repos[%d] %s/%s", i, repo.Owner, repo.Name)
		refName := fmt.Sprintf("validate-repo-%d", i)
		// Mint a real installation token for GitHub App auth (#686): the
		// exchange itself is the preflight — a missing installation or
		// rejected App key fails here with GitHub's diagnosis instead of
		// mid-run. scrubRepositoryError below keeps the token out of output.
		token, err := resolveRepoToken(repo, refName, stores)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
			err = targetRepositoryReachable(ctx, repo, token, stores)
			cancel()
		}
		if err != nil {
			pf(stdout, "REPOSITORY %s: unreachable: %s\n", label, scrubRepositoryError(err, token))
			pf(stdout, "  Check the owner/name, token source, repository access, and network connection.\n")
			addDiagnostic(collectors, file, fmt.Sprintf("/repos/%d", i), "REPO001", string(validate.Error),
				fmt.Sprintf("%s: unreachable: %s", label, scrubRepositoryError(err, token)))
			ok = false
			continue
		}
		pf(stdout, "REPOSITORY %s: reachable\n", label)
		if repo.Provider == "github" {
			warnOnOversizedRepository(label, repo, token, stdout)
		}
	}
	return ok
}

// resolveRepoToken resolves a usable access token for repo — a minted GitHub
// App installation token, a static token ref, or "" when the repo needs
// neither (e.g. non-PAT ADO auth) — shared by the repository preflight
// (checkTargetRepositories) and `goobers doctor --repo` (#916), so both
// preflight the exact credential path a real run would use.
func resolveRepoToken(repo instance.RepoRef, refName string, stores credentials.StoreResolver) (string, error) {
	if repo.GitHubAppAuth() {
		mint, err := newGitHubAppTokenSource(repo, nil, stores)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		defer cancel()
		return mint(ctx)
	}
	if repoUsesToken(repo) {
		resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{
			repo.Token.CredentialTokenRef(refName),
		}, stores)
		if err != nil {
			return "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		defer cancel()
		return resolver.Resolve(ctx, refName)
	}
	return "", nil
}

// warnOnOversizedRepository checks a GitHub target repo's size against
// oversizedRepoThresholdKB and prints an advisory (non-failing) warning
// suggesting the AdditionalRepos partial-checkout remediation (#1547). A
// failure to resolve size (rate limit, transient network error) is not itself
// a validation failure — reachability above already confirmed the repo is
// accessible — so it is reported informationally and does not affect the
// --check-repos exit status.
func warnOnOversizedRepository(label string, repo instance.RepoRef, token string, stdout io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
	defer cancel()
	sizeKB, err := targetRepositorySize(ctx, repo, token)
	if err != nil {
		pf(stdout, "REPOSITORY %s: could not determine repository size: %s\n", label, scrubRepositoryError(err, token))
		return
	}
	if sizeKB <= oversizedRepoThresholdKB {
		return
	}
	pf(stdout, "REPOSITORY %s: WARNING: repository is %d MB, larger than the %d MB checkout-size threshold\n",
		label, sizeKB/1024, oversizedRepoThresholdKB/1024)
	pf(stdout, "  Checking out this repo in full may be slow. Consider a read-only, partial\n"+
		"  AdditionalRepos reference (MGV-10/MGV-11) instead of a full target checkout.\n")
}

func repoUsesToken(repo instance.RepoRef) bool {
	return repo.Provider != "ado" || repo.Auth == nil || repo.Auth.Kind == instance.ADOAuthPAT
}

func gitRepositoryReachable(ctx context.Context, repo instance.RepoRef, token string, stores credentials.StoreResolver) error {
	if repo.Provider == "ado" {
		provider, err := adoauth.Provider(repo, nil, nil, nil, nil, stores)
		if err != nil {
			return err
		}
		return provider.RepositoryReachable(ctx, providers.RepositoryRef{
			Provider: providers.ProviderADO,
			Owner:    repo.Owner,
			Project:  repo.Project,
			Name:     repo.Name,
		})
	}
	if repo.Provider != "github" {
		return fmt.Errorf("provider %q does not support repository preflight", repo.Provider)
	}
	url := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	cmd := exec.Command("git",
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"ls-remote", url,
	)
	cmd.Env = append(gitAuthEnv(token), "GIT_TERMINAL_PROMPT=0")

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	// Spawn in its own session (proc) so the ctx-timeout path below can kill
	// the whole git subprocess tree, not just the direct child.
	tree, err := proc.Start(cmd)
	if err != nil {
		return err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err = <-waitDone:
	case <-ctx.Done():
		_ = tree.Kill()
		select {
		case <-waitDone:
		case <-time.After(repositoryKillWaitDelay):
			// A descendant may have escaped the group while retaining an
			// output pipe. Do not let cmd.Wait block validation indefinitely.
		}
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := strings.TrimSpace(output.String())
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func scrubRepositoryError(err error, token string) string {
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(token))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	return string(scrubber.Scrub([]byte(err.Error())))
}

// harnessAdapterFor is the harness-adapter lookup checkHarnessesAtSources uses.
// Package-level so tests can substitute a fake lookup without depending on a
// real, installed, signed-in Copilot CLI.
var harnessAdapterFor = adapterFor

// checkHarnessesAtSources preflights every distinct harness referenced by set's
// goobers (GBO-011), printing actionable guidance per failure. Returns false
// if any harness failed its preflight.
func checkHarnessesAtSources(
	goobers []apiv1.Goober,
	stdout, stderr io.Writer,
	sourceFile func(apiv1.Goober) string,
	envPassthrough []string,
	harnessCommand map[string][]string,
	modelCredential func(ctx context.Context) (string, error),
	collectors ...*diagnosticCollector,
) bool {
	seen := map[apiv1.Harness]bool{}
	ok := true
	for _, g := range goobers {
		h := g.Spec.Harness
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		file := "."
		if sourceFile != nil {
			file = sourceFile(g)
		}

		adapter, err := harnessAdapterFor(h, envPassthrough, harnessCommand, modelCredential)
		if err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			addDiagnostic(collectors, file, "/spec/harness", "HARNESS001", string(validate.Error), err.Error())
			ok = false
			continue
		}
		// The auth probe is wired into adapterFor itself (#238), so both this
		// check and the automatic daemon-startup preflight verify sign-in, not
		// just CLI presence — a fine-grained PAT lacking the "Copilot Requests"
		// permission (#284) passes --version but fails the probe.
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		_, err = adapter.Preflight(ctx)
		cancel()
		if err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			addDiagnostic(collectors, file, "/spec/harness", "HARNESS003", string(validate.Error), err.Error())
			ok = false
			continue
		}

		pf(stdout, "HARNESS %s: OK\n", h)
	}
	return ok
}

func addDiagnostic(collectors []*diagnosticCollector, file, path, code, severity, message string) {
	if len(collectors) > 0 {
		collectors[0].add(file, path, code, severity, message)
	}
}

// adapterFor returns the registered adapter for a goober-declared harness kind,
// including the instance's configured ambient environment passthrough.
//
// The CopilotAdapter carries copilotAuthCheckArgs so every preflight — the
// operator-invoked `validate --check-harness` AND the automatic daemon-startup
// preflight (preflightAgenticHarnesses) — verifies sign-in, not just CLI
// presence (#238). Both look the harness up through here, so wiring the probe
// once here is what closes #238's "catch a signed-out harness at startup, not
// mid-run" criterion.
func adapterFor(h apiv1.Harness, envPassthrough []string, harnessCommand map[string][]string, modelCredential func(ctx context.Context) (string, error)) (harness.Adapter, error) {
	registry, err := buildHarnessRegistry(nil, envPassthrough, harnessCommand, "", "", false, modelCredential)
	if err != nil {
		return nil, err
	}
	return registry.Get(string(h))
}
