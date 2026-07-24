package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	apivalidate "github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
)

const gatherCIRawLogByteLimit = 0

const gatherCIFailuresHelp = "Usage: goobers gather-ci-failures [path]\n\n" +
	"Enrich this run's remediation brief with failing check names,\n" +
	"conclusions, summaries, and annotations. Passing CI leaves the brief\n" +
	"unchanged and performs no provider API calls. Raw job logs are never\n" +
	"fetched: their explicit per-check volume bound is 0 bytes. [path] is\n" +
	"the instance root, defaulting to GOOBERS_INSTANCE_ROOT. Exit codes:\n" +
	"0 = evidence gathered (or passing-CI no-op), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runGatherCIFailures(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gather-ci-failures", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-ci-failures")
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
	runID, workflow, err := providerRunContext()
	if err != nil {
		return failProviderStage(stderr, "resolve run context", err, remediationBriefResultFile)
	}
	if workflow != "pr-remediation" {
		return failProviderStage(stderr, "resolve run context", fmt.Errorf("workflow is %q, want pr-remediation", workflow), remediationBriefResultFile)
	}

	brief, err := readRemediationBriefArtifact(root, runID, "gather-pr-context")
	if err != nil {
		return failProviderStage(stderr, "read gather-pr-context remediation brief", err, remediationBriefResultFile)
	}
	resultFile := providerInput("resultFile", remediationBriefResultFile)
	if brief.HasFailingCI != "true" {
		if err := writeRemediationBrief(resultFile, brief); err != nil {
			pf(stderr, "error: preserve passing-CI remediation brief: %v\n", err)
			return 2
		}
		pf(stdout, "PR #%s has no failing CI; remediation brief unchanged\n", brief.SelectedNumber)
		return 0
	}

	repo, err := providerRepo(root)
	if err != nil {
		return failProviderStage(stderr, "resolve repository", err, remediationBriefResultFile)
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return failProviderStage(stderr, "resolve GitHub credential", err, remediationBriefResultFile)
	}
	provider := newCachedGitHubProvider(root, token)
	ctx, cancel := providerCommandContext()
	defer cancel()
	failures, err := provider.CIFailures(ctx, repo, brief.GatherPRContext.HeadSHA)
	if err != nil {
		return failProviderStage(stderr, "gather CI failures", err, remediationBriefResultFile)
	}

	checks := make([]apiv1.RemediationCIFailure, 0, len(failures))
	for _, failure := range failures {
		annotations := make([]apiv1.RemediationCIAnnotation, 0, len(failure.Annotations))
		for _, annotation := range failure.Annotations {
			annotations = append(annotations, apiv1.RemediationCIAnnotation{
				Path:      annotation.Path,
				StartLine: annotation.StartLine,
				EndLine:   annotation.EndLine,
				Level:     annotation.Level,
				Title:     annotation.Title,
				Message:   annotation.Message,
			})
		}
		checks = append(checks, apiv1.RemediationCIFailure{
			Name:        failure.Name,
			Conclusion:  failure.Conclusion,
			URL:         failure.URL,
			Summary:     failure.Summary,
			Annotations: annotations,
		})
	}
	brief.GatherCIFailures = &apiv1.RemediationCIFailures{Checks: checks}
	if err := writeRemediationBrief(resultFile, brief); err != nil {
		pf(stderr, "error: write CI-enriched remediation brief: %v\n", err)
		return 2
	}
	pf(stdout, "gathered %d failing CI check(s) for PR #%s\n", len(checks), brief.SelectedNumber)
	return 0
}

func readRemediationBriefArtifact(root, runID, stage string) (apiv1.RemediationBrief, error) {
	runDir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	name := runID + ":" + stage + "/result"
	var ref *journal.Ref
	for i := range events {
		event := &events[i]
		if event.Type == journal.EventArtifactRecorded && event.Name == name && event.Ref != nil {
			ref = event.Ref
		}
	}
	if ref == nil {
		return apiv1.RemediationBrief{}, fmt.Errorf("%s produced no remediation brief artifact", stage)
	}
	data, err := rd.ArtifactBytes(*ref)
	if err != nil {
		return apiv1.RemediationBrief{}, err
	}
	validator, err := apivalidate.New()
	if err != nil {
		return apiv1.RemediationBrief{}, fmt.Errorf("create remediation brief validator: %w", err)
	}
	if err := validator.ValidateJSON(schemas.RemediationBrief, data); err != nil {
		return apiv1.RemediationBrief{}, fmt.Errorf("validate remediation brief: %w", err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		return apiv1.RemediationBrief{}, fmt.Errorf("decode remediation brief: %w", err)
	}
	return brief, nil
}

func writeRemediationBrief(path string, brief apiv1.RemediationBrief) error {
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal remediation brief: %w", err)
	}
	validator, err := apivalidate.New()
	if err != nil {
		return fmt.Errorf("create remediation brief validator: %w", err)
	}
	if err := validator.ValidateJSON(schemas.RemediationBrief, data); err != nil {
		return fmt.Errorf("validate remediation brief: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("result file is required")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
