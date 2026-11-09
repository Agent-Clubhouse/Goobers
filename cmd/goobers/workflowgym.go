package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"text/tabwriter"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workflowgym"
)

const workflowGymHelp = "Usage: goobers workflow-gym <validate|list|replay|tournament|evaluate|promotion-validate> <args>\n\n" +
	"Validate or list a file-backed Workflow Gym benchmark dataset. The path\n" +
	"may name one JSON/YAML task manifest or a directory of manifests. Replay\n" +
	"runs one task against historical fixture and journal inputs, entirely offline.\n" +
	"Tournament compares candidate result manifests over one identical task roster.\n" +
	"Evaluate applies confidence and regression gates to tournament JSON output.\n" +
	"Promotion-validate checks promotion evidence against Workflow CD policy without\n" +
	"mutating production and emits an auditable validation receipt.\n"

const workflowGymValidateHelp = "Usage: goobers workflow-gym validate <path>\n\n" +
	"Validate a Workflow Gym dataset and print its version, deterministic\n" +
	"digest, and task count.\n"

const workflowGymListHelp = "Usage: goobers workflow-gym list <path>\n\n" +
	"Validate a Workflow Gym dataset, print its version, deterministic digest,\n" +
	"and task count, then list tasks in stable task ID order.\n"

const workflowGymReplayHelp = "Usage: goobers workflow-gym replay <task-manifest> <provider-fixture> <journal-dir>\n\n" +
	"Replay one historical task twice through the offline benchmark harness and\n" +
	"write its deterministic replay manifest as JSON. The replay reads the\n" +
	"fixture's work item and re-issues the journal's recorded mutations through\n" +
	"the side-effect sandbox; the manifest lists only effects the sandbox\n" +
	"observed, as deterministic, virtualized, or unavailable.\n"

const workflowGymTournamentHelp = "Usage: goobers workflow-gym tournament <dataset> <authoritative> <candidate> [candidate...]\n\n" +
	"Compare authoritative and candidate EffectiveVersion/genome result manifests\n" +
	"over exactly the same task IDs and write a deterministic paired report as JSON.\n" +
	"Each WorkflowGymCandidate JSON/YAML manifest supplies datasetDigest (must match\n" +
	"the loaded dataset), identity.effectiveVersion, identity.genomeDigest, and a\n" +
	"non-empty list of task results with evaluatorVersion (must match the task),\n" +
	"status, costUsd, durationMs, and failureClass. The authoritative manifest must\n" +
	"cover every task; omitted candidate results are recorded as failures.\n"

const workflowGymEvaluateHelp = "Usage: goobers workflow-gym evaluate [--min-samples N] [--regression-threshold RATE]\n" +
	"       <dataset> <tournament-report>\n\n" +
	"Evaluate paired tournament JSON with per-slice sample floors, 95% confidence\n" +
	"intervals, a pass-rate regression threshold, and a Pareto leaderboard. Train\n" +
	"and eval tasks determine the gate and ranking; holdout results are reported but\n" +
	"never used for selection. Missing candidate results never count as measured\n" +
	"evidence. The command writes a machine-readable JSON report to stdout.\n\n" +
	"Options:\n" +
	"  --min-samples N              Required samples in each selection slice (default 10)\n" +
	"  --regression-threshold RATE  Tolerated pass-rate regression from 0 to 1 (default 0.05)\n\n" +
	"Exit codes: 0 = pass, 1 = fail (regression meets the threshold), 2 = usage\n" +
	"error, 3 = inconclusive (insufficient or missing evidence), 4 = input or\n" +
	"processing error (unreadable or inconsistent dataset or tournament report).\n"

const workflowGymPromotionValidateHelp = "Usage: goobers workflow-gym promotion-validate <evidence> <policy>\n\n" +
	"Validate a WorkflowGymPromotionEvidence JSON/YAML artifact against the current\n" +
	"WorkflowGymPromotionPolicy. Each benchmark, shadow, or holdout slot names its\n" +
	"stage, an evaluate JSON report path (relative to the evidence file), and that\n" +
	"report's canonical artifactDigest; verdicts and samples come from the report.\n" +
	"Validation is read-only and writes a WorkflowGymPromotionValidation receipt\n" +
	"with verdict, reasons, and digests to stdout, on rejection as well as success.\n\n" +
	"Exit codes: 0 = pass, 1 = fail (regressed, mismatched, stale, or reused\n" +
	"evidence), 2 = usage error, 3 = inconclusive (missing required evidence or too\n" +
	"few samples), 4 = input error (unreadable or malformed evidence, policy, or report).\n"

// Exit codes for goobers workflow-gym evaluate, distinct per gate outcome so
// automation can tell a regression from missing evidence or a broken input.
const (
	workflowGymEvaluateExitPass         = 0
	workflowGymEvaluateExitFail         = 1
	workflowGymEvaluateExitUsage        = 2
	workflowGymEvaluateExitInconclusive = 3
	workflowGymEvaluateExitInputError   = 4
)

func workflowGymCLICommand() cliCommand {
	return groupCommand(
		"workflow-gym",
		runWorkflowGym,
		subcommand("workflow-gym validate", "validate", apicontract.ActionReadOnlyNavigation, runWorkflowGymValidate).
			withHelp("validate a file-backed benchmark dataset", workflowGymValidateHelp).
			withExamples("goobers workflow-gym validate ./benchmark-tasks"),
		subcommand("workflow-gym list", "list", apicontract.ActionReadOnlyNavigation, runWorkflowGymList).
			withHelp("list a file-backed benchmark dataset", workflowGymListHelp).
			withExamples("goobers workflow-gym list ./benchmark-tasks"),
		subcommand("workflow-gym replay", "replay", apicontract.ActionReadOnlyNavigation, runWorkflowGymReplay).
			withHelp("replay a historical benchmark task offline", workflowGymReplayHelp).
			withExamples("goobers workflow-gym replay ./task.yaml ./provider.json ./journal"),
		subcommand("workflow-gym tournament", "tournament", apicontract.ActionReadOnlyNavigation, runWorkflowGymTournament).
			withHelp("compare candidates over an identical benchmark task suite", workflowGymTournamentHelp).
			withExamples("goobers workflow-gym tournament ./tasks ./authoritative.yaml ./candidate.yaml"),
		subcommand("workflow-gym evaluate", "evaluate", apicontract.ActionReadOnlyNavigation, runWorkflowGymEvaluate).
			withHelp("gate and rank paired tournament results", workflowGymEvaluateHelp).
			withExamples("goobers workflow-gym evaluate --min-samples 20 --regression-threshold 0.05 ./tasks ./tournament.json"),
		subcommand("workflow-gym promotion-validate", "promotion-validate", apicontract.ActionReadOnlyNavigation, runWorkflowGymPromotionValidate).
			withHelp("validate promotion evidence for Workflow CD", workflowGymPromotionValidateHelp).
			withExamples("goobers workflow-gym promotion-validate ./evidence.yaml ./policy.yaml"),
	).
		withSynopsis("workflow-gym <validate|list|replay|tournament|evaluate|promotion-validate> <args>").
		withHelp("validate, replay, compare, evaluate, or hand off Workflow Gym evidence", workflowGymHelp).
		withExamples("goobers workflow-gym validate ./benchmark-tasks", "goobers workflow-gym list ./benchmark-tasks",
			"goobers workflow-gym replay ./task.yaml ./provider.json ./journal",
			"goobers workflow-gym tournament ./tasks ./authoritative.yaml ./candidate.yaml",
			"goobers workflow-gym evaluate ./tasks ./tournament.json",
			"goobers workflow-gym promotion-validate ./evidence.yaml ./policy.yaml")
}

func runWorkflowGym(args []string, _, stderr io.Writer) int {
	if len(args) > 0 {
		pf(stderr, "error: unknown workflow-gym command %q\n", args[0])
	}
	pf(stderr, "%s", workflowGymHelp)
	return 2
}

func runWorkflowGymValidate(args []string, stdout, stderr io.Writer) int {
	registry, code := loadWorkflowGymCLI(args, stderr)
	if code != 0 {
		return code
	}
	printWorkflowGymSummary(stdout, registry)
	return 0
}

func runWorkflowGymList(args []string, stdout, stderr io.Writer) int {
	registry, code := loadWorkflowGymCLI(args, stderr)
	if code != 0 {
		return code
	}
	printWorkflowGymSummary(stdout, registry)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, task := range registry.Tasks {
		pf(tw, "%s\t%s\n", task.TaskID, task.Split)
	}
	if err := tw.Flush(); err != nil {
		pf(stderr, "error: write task list: %v\n", err)
		return 1
	}
	return 0
}

func runWorkflowGymReplay(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		pf(stderr, "%s", workflowGymReplayHelp)
		return 2
	}
	registry, err := workflowgym.Load(args[0])
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if len(registry.Tasks) != 1 {
		pf(stderr, "error: replay requires exactly one task manifest, got %d\n", len(registry.Tasks))
		return 1
	}
	manifest, err := workflowgym.Replay(context.Background(), workflowgym.ReplayRequest{
		Task:                registry.Tasks[0],
		ProviderFixturePath: args[1],
		JournalDir:          args[2],
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		pf(stderr, "error: write replay manifest: %v\n", err)
		return 1
	}
	return 0
}

func runWorkflowGymTournament(args []string, stdout, stderr io.Writer) int {
	if len(args) < 3 {
		pf(stderr, "%s", workflowGymTournamentHelp)
		return 2
	}
	registry, err := workflowgym.Load(args[0])
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	authoritative, err := workflowgym.LoadTournamentCandidate(args[1])
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	candidates := make([]workflowgym.TournamentCandidateManifest, 0, len(args)-2)
	for _, path := range args[2:] {
		candidate, err := workflowgym.LoadTournamentCandidate(path)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		candidates = append(candidates, candidate)
	}
	report, err := workflowgym.RunTournament(registry, authoritative, candidates)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		pf(stderr, "error: write tournament report: %v\n", err)
		return 1
	}
	return 0
}

func runWorkflowGymEvaluate(args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("workflow-gym evaluate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	minSamples := flags.Int("min-samples", workflowgym.DefaultEvaluationMinSamples, "")
	regressionThreshold := flags.Float64("regression-threshold", workflowgym.DefaultRegressionThreshold, "")
	flags.Usage = func() { pf(stderr, "%s", workflowGymEvaluateHelp) }
	if err := flags.Parse(args); err != nil {
		return workflowGymEvaluateExitUsage
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return workflowGymEvaluateExitUsage
	}
	report, err := evaluateWorkflowGymTournament(flags.Arg(0), flags.Arg(1), workflowgym.EvaluationConfig{
		MinSamples:          *minSamples,
		RegressionThreshold: *regressionThreshold,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return workflowGymEvaluateExitInputError
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		pf(stderr, "error: write evaluation report: %v\n", err)
		return workflowGymEvaluateExitInputError
	}
	return workflowGymVerdictExit(report.Gate.Verdict)
}

func runWorkflowGymPromotionValidate(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		pf(stderr, "%s", workflowGymPromotionValidateHelp)
		return workflowGymEvaluateExitUsage
	}
	validation, err := workflowgym.ValidatePromotionFiles(args[0], args[1])
	if err == nil {
		err = workflowgym.WritePromotionValidation(stdout, validation)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return workflowGymEvaluateExitInputError
	}
	return workflowGymVerdictExit(validation.Verdict)
}

func workflowGymVerdictExit(verdict workflowgym.EvaluationVerdict) int {
	switch verdict {
	case workflowgym.EvaluationPass:
		return workflowGymEvaluateExitPass
	case workflowgym.EvaluationFail:
		return workflowGymEvaluateExitFail
	default:
		return workflowGymEvaluateExitInconclusive
	}
}

func evaluateWorkflowGymTournament(datasetPath, reportPath string, config workflowgym.EvaluationConfig) (workflowgym.EvaluationReport, error) {
	registry, err := workflowgym.Load(datasetPath)
	if err != nil {
		return workflowgym.EvaluationReport{}, err
	}
	tournament, err := workflowgym.LoadTournamentReport(reportPath)
	if err != nil {
		return workflowgym.EvaluationReport{}, err
	}
	return workflowgym.EvaluateTournament(registry, tournament, config)
}

func loadWorkflowGymCLI(args []string, stderr io.Writer) (workflowgym.Registry, int) {
	if len(args) != 1 {
		pf(stderr, "%s", workflowGymHelp)
		return workflowgym.Registry{}, 2
	}
	registry, err := workflowgym.Load(args[0])
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return workflowgym.Registry{}, 1
	}
	return registry, 0
}

func printWorkflowGymSummary(stdout io.Writer, registry workflowgym.Registry) {
	pf(stdout, "datasetVersion: %s\n", registry.DatasetVersion)
	pf(stdout, "datasetDigest: %s\n", registry.Digest)
	pf(stdout, "taskCount: %d\n", len(registry.Tasks))
}
