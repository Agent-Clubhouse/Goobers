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

const workflowGymHelp = "Usage: goobers workflow-gym <validate|list|replay|tournament|evaluate> <args>\n\n" +
	"Validate or list a file-backed Workflow Gym benchmark dataset. The path\n" +
	"may name one JSON/YAML task manifest or a directory of manifests. Replay\n" +
	"runs one task against historical fixture and journal inputs, entirely offline.\n" +
	"Tournament compares candidate result manifests over one identical task roster.\n" +
	"Evaluate applies confidence and regression gates to tournament JSON output.\n"

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

const workflowGymEvaluateHelp = "Usage: goobers workflow-gym evaluate [OPTIONS] <dataset> <tournament-report>\n\n" +
	"Evaluate paired tournament JSON with per-slice sample floors, 95% confidence\n" +
	"intervals, a pass-rate regression threshold, and a Pareto leaderboard. Train\n" +
	"and eval tasks determine the gate and ranking; holdout results are reported but\n" +
	"never used for selection. The command writes a machine-readable JSON report and\n" +
	"exits non-zero for fail or inconclusive verdicts.\n\n" +
	"Options:\n" +
	"  --min-samples N              Required samples in each selection slice (default 10)\n" +
	"  --regression-threshold RATE  Tolerated pass-rate regression from 0 to 1 (default 0.05)\n"

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
	).
		withSynopsis("workflow-gym <validate|list|replay|tournament|evaluate> <args>").
		withHelp("validate, replay, compare, or evaluate Workflow Gym benchmark tasks", workflowGymHelp).
		withExamples("goobers workflow-gym validate ./benchmark-tasks", "goobers workflow-gym list ./benchmark-tasks",
			"goobers workflow-gym replay ./task.yaml ./provider.json ./journal",
			"goobers workflow-gym tournament ./tasks ./authoritative.yaml ./candidate.yaml",
			"goobers workflow-gym evaluate ./tasks ./tournament.json")
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
	flags := flag.NewFlagSet("workflow-gym evaluate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	minSamples := flags.Int("min-samples", workflowgym.DefaultEvaluationMinSamples, "")
	regressionThreshold := flags.Float64("regression-threshold", workflowgym.DefaultRegressionThreshold, "")
	flags.Usage = func() { pf(stderr, "%s", workflowGymEvaluateHelp) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return 2
	}
	registry, err := workflowgym.Load(flags.Arg(0))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	tournament, err := workflowgym.LoadTournamentReport(flags.Arg(1))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	report, err := workflowgym.EvaluateTournament(registry, tournament, workflowgym.EvaluationConfig{
		MinSamples:          *minSamples,
		RegressionThreshold: *regressionThreshold,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		pf(stderr, "error: write evaluation report: %v\n", err)
		return 1
	}
	if report.Gate.Verdict != workflowgym.EvaluationPass {
		return 1
	}
	return 0
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
