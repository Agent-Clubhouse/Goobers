package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// gatherCIRawLogByteLimit is the raw job-log volume fetched per check on
// GitHub and Gitea, whose check annotations carry the diagnosis. Azure DevOps
// has no annotations, so its arm reads bounded excerpts of the failed steps'
// logs instead (providers.ADOCIEvidenceBounds).
const gatherCIRawLogByteLimit = 0

// remediationBriefArtifact names the thing an upstream stage owed this one, for
// the operator-facing message an upstreamArtifactError builds (#4121).
const remediationBriefArtifact = "remediation brief artifact"

const gatherCIFailuresHelp = "Usage: goobers gather-ci-failures [path]\n\n" +
	"Enrich this run's remediation brief with failing check names,\n" +
	"conclusions, summaries, and annotations. Passing CI leaves the brief\n" +
	"unchanged and performs no provider API calls. On GitHub and Gitea raw\n" +
	"job logs are never fetched: their per-check volume bound is 0 bytes.\n" +
	"On Azure DevOps each failing build or status policy is traced to its\n" +
	"build, and the build's failed jobs and tasks are reported with their\n" +
	"issues and a bounded excerpt of each failed step's log; every check's\n" +
	"summary grades its evidence (complete, partial_bound, partial_provider,\n" +
	"unsupported, failed or stale), so an external status or a missing log\n" +
	"is explicit. [path] is the instance root, defaulting to\n" +
	"GOOBERS_INSTANCE_ROOT. Exit codes:\n" +
	"0 = evidence gathered (or passing-CI no-op), 1 = business error,\n" +
	"2 = usage/IO error.\n"

func runGatherCIFailures(args []string, stdout, stderr io.Writer) int {
	env, ok, exitCode := parseProviderStageCommandRoot(args, "gather-ci-failures", stderr)
	if !ok {
		return exitCode
	}
	root := env.root
	runID, _, err := providerRunContext()
	if err != nil {
		return failProviderStage(stderr, "resolve run context", err, remediationBriefResultFile)
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

	env, err = loadProviderStageEnv(root)
	if err != nil {
		return failProviderStage(stderr, "resolve repository", err, remediationBriefResultFile)
	}
	repo := env.repo
	ctx, cancel := providerCommandContext()
	defer cancel()
	failures, step, err := gatherCIFailureDetails(ctx, root, repo, brief)
	if err != nil {
		return failProviderStage(stderr, step, err, remediationBriefResultFile)
	}

	checks := remediationCIFailureChecks(failures)
	brief.GatherCIFailures = &apiv1.RemediationCIFailures{Checks: checks}
	brief.Integrity = apiv1.WeakestIntegrity(brief.Integrity, apiv1.IntegrityUnapproved)
	if err := writeRemediationBrief(resultFile, brief); err != nil {
		pf(stderr, "error: write CI-enriched remediation brief: %v\n", err)
		return 2
	}
	pf(stdout, "gathered %d failing CI check(s) for PR #%s\n", len(checks), brief.SelectedNumber)
	return 0
}

// gatherCIFailureDetails reads the failing-CI evidence for the brief's pull
// request. GitHub and Gitea report CI per commit, so they are asked for the
// brief's head SHA exactly as before. On failure it also names the step that
// failed, for failProviderStage.
func gatherCIFailureDetails(ctx context.Context, root string, repo providers.RepositoryRef, brief apiv1.RemediationBrief) ([]providers.CIFailureDetail, string, error) {
	if repo.Provider == providers.ProviderADO {
		return gatherADOCIFailures(ctx, root, repo, brief)
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return nil, "resolve GitHub credential", err
	}
	provider, err := remediationStageProvider(root, repo, token, true)
	if err != nil {
		return nil, "build remediation provider", err
	}
	failures, err := provider.CIFailures(ctx, repo, brief.GatherPRContext.HeadSHA)
	if err != nil {
		return nil, "gather CI failures", err
	}
	return failures, "", nil
}

// gatherADOCIFailures is the Azure DevOps arm (ADO-N22, design
// ado-parity-dsl-2-0.md §3.4; #5652): native evidence from the pull
// request's rejected policy evaluations, each traced to its build's failed
// jobs, tasks, issues and bounded log excerpts, and graded for completeness
// (providers.PullRequestCIFailures). It builds its provider through the same
// narrow surface as the review-thread stages, so the ADO stage factory
// resolves the credential itself.
//
// Policy evaluations belong to the pull request, not to a commit. When the
// head ADO reports differs from the brief's head, the pull request moved
// after gather-pr-context read it, so each finding is marked stale rather
// than presented as evidence about the brief's head.
func gatherADOCIFailures(ctx context.Context, root string, repo providers.RepositoryRef, brief apiv1.RemediationBrief) ([]providers.CIFailureDetail, string, error) {
	reader, err := reviewThreadStageSurface[providers.PullRequestCIFailureReader](root, repo, false)
	if err != nil {
		return nil, "build remediation provider", err
	}
	evidence, err := reader.PullRequestCIFailures(ctx, repo, brief.SelectedNumber)
	if err != nil {
		return nil, "gather CI failures", err
	}
	want := strings.TrimSpace(brief.GatherPRContext.HeadSHA)
	if want == "" || strings.EqualFold(evidence.HeadSHA, want) {
		return evidence.Failures, "", nil
	}
	stale := fmt.Sprintf("STALE: Azure DevOps evaluated head %s, not this brief's head %s", evidence.HeadSHA, want)
	if len(evidence.Failures) == 0 {
		// No rejected CI policy at the new head (a re-queued build, say) is
		// not evidence that the brief's head is clean: say so visibly.
		return []providers.CIFailureDetail{staleADOCIEvidence(stale)}, "", nil
	}
	for i := range evidence.Failures {
		summary := stale
		if evidence.Failures[i].Summary != "" {
			summary += "; " + evidence.Failures[i].Summary
		}
		evidence.Failures[i].Summary = summary
		evidence.Failures[i].Evidence = providers.CIEvidenceStale
	}
	return evidence.Failures, "", nil
}

// staleADOCIEvidence is the single marker check recorded when the pull
// request moved and ADO reports no rejected CI policy at its new head, so the
// empty evidence is not read as current.
func staleADOCIEvidence(stale string) providers.CIFailureDetail {
	return providers.CIFailureDetail{
		CheckDetail: providers.CheckDetail{
			Name:       "Azure DevOps policy evidence",
			State:      providers.CheckStatePending,
			Conclusion: "stale",
			Summary:    stale + "; no rejected CI policy is reported at the evaluated head",
		},
		Annotations: []providers.CheckAnnotation{},
		Evidence:    providers.CIEvidenceStale,
	}
}

// remediationCIFailureChecks maps provider CI evidence to the brief's shape.
func remediationCIFailureChecks(failures []providers.CIFailureDetail) []apiv1.RemediationCIFailure {
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
	return checks
}

func readRemediationBriefArtifact(root, runID, stage string) (apiv1.RemediationBrief, error) {
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(stage, remediationBriefArtifact, err)
	}
	events, err := rd.Events()
	if err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(stage, remediationBriefArtifact, err)
	}
	name := stage + "/result"
	var ref *journal.Ref
	for i := range events {
		event := &events[i]
		if event.Type == journal.EventArtifactRecorded && stageArtifactName(runID, event.Name) == name && event.Ref != nil {
			ref = event.Ref
		}
	}
	if ref == nil {
		return apiv1.RemediationBrief{}, upstreamArtifactMissing(stage, remediationBriefArtifact)
	}
	data, err := rd.ArtifactBytes(*ref)
	if err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(stage, remediationBriefArtifact, err)
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(stage, remediationBriefArtifact, err)
	}
	var brief apiv1.RemediationBrief
	if err := json.Unmarshal(data, &brief); err != nil {
		return apiv1.RemediationBrief{}, upstreamArtifactUnreadable(stage, remediationBriefArtifact,
			fmt.Errorf("decode remediation brief: %w", err))
	}
	return brief, nil
}

func writeRemediationBrief(path string, brief apiv1.RemediationBrief) error {
	data, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		return &remediationBriefError{
			kind: remediationBriefErrorMarshal,
			err:  fmt.Errorf("marshal remediation brief: %w", err),
		}
	}
	if err := validateRemediationBriefJSON(data); err != nil {
		return &remediationBriefError{kind: remediationBriefErrorValidate, err: err}
	}
	if strings.TrimSpace(path) == "" {
		return &remediationBriefError{
			kind: remediationBriefErrorWrite,
			err:  fmt.Errorf("result file is required"),
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return &remediationBriefError{
			kind: remediationBriefErrorWrite,
			err:  fmt.Errorf("write %s: %w", path, err),
		}
	}
	return nil
}

type remediationBriefErrorKind uint8

const (
	remediationBriefErrorMarshal remediationBriefErrorKind = iota
	remediationBriefErrorValidate
	remediationBriefErrorWrite
)

type remediationBriefError struct {
	kind remediationBriefErrorKind
	err  error
}

func (e *remediationBriefError) Error() string {
	return e.err.Error()
}

func (e *remediationBriefError) Unwrap() error {
	return e.err
}

func remediationBriefErrorExitCode(err error, writeErrorExitCode int) int {
	var briefErr *remediationBriefError
	if errors.As(err, &briefErr) && briefErr.kind == remediationBriefErrorWrite {
		return writeErrorExitCode
	}
	return 1
}

func writeGatherRemediationBrief(stderr io.Writer, path string, brief apiv1.RemediationBrief, writeErrorExitCode int) int {
	if err := writeRemediationBrief(path, brief); err != nil {
		pf(stderr, "error: %v\n", err)
		return remediationBriefErrorExitCode(err, writeErrorExitCode)
	}
	return 0
}

func validateRemediationBriefJSON(data []byte) error {
	if err := validateSchemaJSON(schemas.RemediationBrief, data); err != nil {
		return fmt.Errorf("validate remediation brief: %w", err)
	}
	return nil
}
