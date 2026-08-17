package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/journal"
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
	Valid                    bool     `json:"valid"`
	PlanDigest               string   `json:"planDigest,omitempty"`
	Errors                   []string `json:"errors,omitempty"`
	Conflict                 bool     `json:"conflict"`
	ConflictReason           string   `json:"conflictReason,omitempty"`
	UnresolvedDecision       bool     `json:"unresolvedDecision"`
	UnresolvedDecisionReason string   `json:"unresolvedDecisionReason,omitempty"`
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

	plan, err := readDecompositionInput[decomposition.Plan](root, providerInput("planFile", "plan.json"), "plan.json", "design-slices", "/plan.json")
	if err != nil {
		pf(stderr, "error: read plan: %v\n", err)
		return 1
	}
	selection, err := readDecompositionInput[decomposition.Selection](root, providerInput("selectionFile", "selection.json"), "selection.json", "select-source", "/result")
	if err != nil {
		pf(stderr, "error: read selection: %v\n", err)
		return 1
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	issueProvider, err := newProviderForStage(root, repo, true, withStageProviderCache())
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

	conflictReason := ""
	if result.Conflict != nil {
		conflictReason = result.Conflict.Reason
	}
	data, err := json.Marshal(validatePlanResult{
		Valid:                    result.Valid,
		PlanDigest:               planDigest,
		Errors:                   result.Errors,
		Conflict:                 result.Conflict != nil,
		ConflictReason:           conflictReason,
		UnresolvedDecision:       result.UnresolvedDecision,
		UnresolvedDecisionReason: plan.UnresolvedDecision,
	})
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

func readDecompositionInput[T any](root, path, defaultPath, stage, artifactSuffix string) (T, error) {
	value, err := readValidatePlanInput[T](path)
	if err == nil || path != defaultPath || !errors.Is(err, os.ErrNotExist) {
		return value, err
	}
	runID := os.Getenv("GOOBERS_RUN_ID")
	if runID == "" {
		return value, err
	}
	runDir, runErr := runDirFor(layoutFor(root), runID)
	if runErr != nil {
		return value, err
	}
	reader, runErr := journal.OpenRead(runDir)
	if runErr != nil {
		return value, fmt.Errorf("open run journal for %s input: %w", stage, runErr)
	}
	events, runErr := reader.Events()
	if runErr != nil {
		return value, fmt.Errorf("read run journal for %s input: %w", stage, runErr)
	}
	ref, ok := decompositionStageArtifact(events, stage, artifactSuffix)
	if !ok {
		return value, err
	}
	data, runErr := reader.ArtifactBytes(ref)
	if runErr != nil {
		return value, fmt.Errorf("read %s artifact: %w", stage, runErr)
	}
	if runErr := json.Unmarshal(data, &value); runErr != nil {
		return value, fmt.Errorf("parse %s artifact: %w", stage, runErr)
	}
	return value, nil
}

func decompositionStageArtifact(events []journal.Event, stage, nameSuffix string) (journal.Ref, bool) {
	artifacts := make(map[string]journal.Ref)
	for _, event := range events {
		if event.Type == journal.EventArtifactRecorded && event.Ref != nil && strings.HasSuffix(event.Name, nameSuffix) {
			artifacts[event.Ref.Digest] = *event.Ref
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventStageFinished || event.Stage != stage || event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		for _, ref := range event.Artifacts {
			if artifact, ok := artifacts[ref.Digest]; ok {
				return artifact, true
			}
		}
	}
	return journal.Ref{}, false
}
