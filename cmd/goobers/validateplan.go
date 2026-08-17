package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/internal/decomposition"
)

const validatePlanHelp = "Usage: goobers validate-plan [path]\n\n" +
	"validate-plan is the decomposition workflow's `validate-plan` stage\n" +
	"(docs/design/decomposition-workflow.md §3.2/§4, DEC-2). It reads the\n" +
	"decomposition plan design-slices produced and the selection artifact\n" +
	"select-source produced, checks the plan's schema version, its binding to\n" +
	"the selector artifact, every child's shape, the dependency graph, and the\n" +
	"label allowlist, then re-fetches the live parent to detect whether it\n" +
	"changed since selection. It never mutates the provider.\n\n" +
	"Exit codes: 0 = the plan is valid, or invalid/conflicting (the result file\n" +
	"reports which — not a business error) / 1 = business error (provider/\n" +
	"credential/config/IO error) / 2 = usage error.\n"

// validatePlanResult is validate-plan's declared result file shape: a plain
// bool plus the accumulated findings, so design-slices' repass context (a
// valid plan) and park-for-human's routing (a conflict) can both read it
// without parsing prose.
type validatePlanResult struct {
	Valid      bool                          `json:"valid"`
	PlanDigest string                        `json:"planDigest,omitempty"`
	Errors     []string                      `json:"errors,omitempty"`
	Conflict   *decomposition.ParentConflict `json:"conflict,omitempty"`
}

func runValidatePlan(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("validate-plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "validate-plan")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	plan, err := readValidatePlanInput[decomposition.Plan](providerInput("planFile", "plan.json"))
	if err != nil {
		pf(stderr, "error: read plan: %v\n", err)
		return 1
	}
	selection, err := readValidatePlanInput[decomposition.Selection](providerInput("selectionFile", "selection.json"))
	if err != nil {
		pf(stderr, "error: read selection: %v\n", err)
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	issueProvider, err := newProviderForStage(root, repo, false, withStageProviderCache())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()

	item, err := issueProvider.GetWorkItem(ctx, repo, selection.Parent.ID)
	if err != nil {
		return failProviderStage(stderr, fmt.Sprintf("get parent %s", selection.Parent.ID), err, "plan-validation.json")
	}

	live := decomposition.LiveParentSnapshot{
		ID:     item.ID,
		Title:  item.Title,
		Body:   item.Body,
		Labels: decompositionDigestLabels(item.Labels),
		State:  item.State,
	}
	result := decomposition.ValidatePlan(plan, selection, live)
	planDigest := ""
	if result.Valid {
		planDigest, err = decomposition.PlanDigest(plan)
		if err != nil {
			pf(stderr, "error: digest validated plan: %v\n", err)
			return 1
		}
	}

	data, err := json.Marshal(validatePlanResult{Valid: result.Valid, PlanDigest: planDigest, Errors: result.Errors, Conflict: result.Conflict})
	if err != nil {
		pf(stderr, "error: marshal plan-validation result: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "plan-validation.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}

	switch {
	case result.Valid:
		pf(stdout, "plan for parent %s is valid\n", selection.Parent.ID)
	case result.Conflict != nil:
		pf(stdout, "plan for parent %s conflicts with the live parent: %s\n", selection.Parent.ID, result.Conflict.Reason)
	default:
		pf(stdout, "plan for parent %s is invalid (%d finding(s))\n", selection.Parent.ID, len(result.Errors))
	}
	return 0
}

func readValidatePlanInput[T any](path string) (T, error) {
	var value T
	data, err := os.ReadFile(path)
	if err != nil {
		return value, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}
