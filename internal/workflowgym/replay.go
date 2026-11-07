package workflowgym

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

// ReplayRequest selects one task and its historical, read-only replay inputs.
// Act scripts the candidate's work through the benchmark sandbox; nil replays
// the recorded session itself (see recordedSessionAct), which is what the CLI
// does.
type ReplayRequest struct {
	Task                apiv1.WorkflowGymTaskManifest
	ProviderFixturePath string
	JournalDir          string
	Act                 harness.BenchmarkAct
}

// Replay runs the historical task twice in fresh workspaces and returns a
// canonical manifest describing the effects the sandbox observed and the
// replay's determinism. Recorded exchanges the replay never requested are not
// effects and are omitted.
func Replay(ctx context.Context, req ReplayRequest) (apiv1.WorkflowGymReplayManifest, error) {
	if err := validateTask(req.Task); err != nil {
		return apiv1.WorkflowGymReplayManifest{}, fmt.Errorf("workflow gym replay: task manifest: %w", err)
	}
	if req.ProviderFixturePath == "" {
		return apiv1.WorkflowGymReplayManifest{}, errors.New("workflow gym replay: provider fixture path is required")
	}
	if req.JournalDir == "" {
		return apiv1.WorkflowGymReplayManifest{}, errors.New("workflow gym replay: journal directory is required")
	}
	fixture, err := harness.LoadBenchmarkFixture(req.ProviderFixturePath, req.JournalDir)
	if err != nil {
		return apiv1.WorkflowGymReplayManifest{}, err
	}
	if req.Act == nil {
		req.Act = recordedSessionAct(fixture)
	}

	first, err := runReplay(ctx, req, fixture)
	if err != nil {
		return apiv1.WorkflowGymReplayManifest{}, err
	}
	second, err := runReplay(ctx, req, fixture)
	if err != nil {
		return apiv1.WorkflowGymReplayManifest{}, err
	}

	taskJSON, err := json.Marshal(req.Task)
	if err != nil {
		return apiv1.WorkflowGymReplayManifest{}, fmt.Errorf("workflow gym replay: encode task identity: %w", err)
	}
	firstEffects := effectLedger(first.report)
	manifest := apiv1.WorkflowGymReplayManifest{
		APIVersion: apiv1.WorkflowGymTaskAPIVersion,
		Kind:       apiv1.WorkflowGymReplayKind,
		Candidate: apiv1.WorkflowGymCandidateIdentity{
			TaskID:           req.Task.TaskID,
			DatasetVersion:   req.Task.DatasetVersion,
			TaskDigest:       journal.Digest(taskJSON),
			EvaluatorVersion: req.Task.EvaluatorVersion,
		},
		Environment: req.Task.CandidateEnvironment,
		Sources: apiv1.WorkflowGymReplaySources{
			ProviderFixture: filepath.Clean(req.ProviderFixturePath),
			Journal:         filepath.Clean(req.JournalDir),
		},
		Status:                 first.report.Status,
		JournalDigest:          journal.Digest(first.report.Journal),
		Effects:                firstEffects,
		VirtualizationGaps:     virtualizationGaps(first.report),
		NondeterminismFindings: nondeterminismFindings(first, second, firstEffects, effectLedger(second.report)),
	}
	return manifest, nil
}

// effectLedger projects what the sandbox observed during one replay:
// recorded artifacts it replayed are deterministic, recorded exchanges that
// answered a request are virtualized (repeat reads of one exchange collapse
// into one entry), and every refused effect is unavailable.
func effectLedger(report harness.BenchmarkReport) []apiv1.WorkflowGymReplayEffect {
	effects := make([]apiv1.WorkflowGymReplayEffect, 0, len(report.Served)+len(report.Violations))
	for _, event := range report.Events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil {
			continue
		}
		effects = append(effects, apiv1.WorkflowGymReplayEffect{
			Disposition: apiv1.WorkflowGymEffectDeterministic,
			Effect:      string(harness.BenchmarkEffectArtifact),
			Target:      event.Name,
			Detail:      event.Ref.Digest,
		})
	}
	reads := make(map[string]int)
	for _, served := range report.Served {
		if served.Effect == harness.BenchmarkEffectProviderRead {
			reads[served.Exchange]++
		}
	}
	collapsed := make(map[string]bool)
	for _, served := range report.Served {
		detail := served.Exchange
		if served.Effect == harness.BenchmarkEffectProviderRead {
			if collapsed[served.Exchange] {
				continue
			}
			collapsed[served.Exchange] = true
			if n := reads[served.Exchange]; n > 1 {
				detail = fmt.Sprintf("%s (served %d reads)", served.Exchange, n)
			}
		}
		effects = append(effects, apiv1.WorkflowGymReplayEffect{
			Disposition: apiv1.WorkflowGymEffectVirtualized,
			Effect:      string(served.Effect),
			Target:      served.Method + " " + served.Path,
			Detail:      detail,
		})
	}
	for _, violation := range report.Violations {
		effects = append(effects, apiv1.WorkflowGymReplayEffect{
			Disposition: apiv1.WorkflowGymEffectUnavailable,
			Effect:      string(violation.Effect),
			Target:      violation.Target,
			Detail:      violation.Message,
		})
	}
	return effects
}

func virtualizationGaps(report harness.BenchmarkReport) []apiv1.WorkflowGymReplayFinding {
	gaps := make([]apiv1.WorkflowGymReplayFinding, 0)
	for _, violation := range report.Violations {
		if violation.Code == harness.BenchmarkErrorVirtualizationGap {
			gaps = append(gaps, apiv1.WorkflowGymReplayFinding{Code: violation.Code, Detail: violation.Message})
		}
	}
	return gaps
}

func nondeterminismFindings(first, second replayResult, firstEffects, secondEffects []apiv1.WorkflowGymReplayEffect) []apiv1.WorkflowGymReplayFinding {
	findings := make([]apiv1.WorkflowGymReplayFinding, 0)
	if !bytes.Equal(first.report.Journal, second.report.Journal) {
		findings = append(findings, apiv1.WorkflowGymReplayFinding{
			Code: "journal_mismatch",
			Detail: fmt.Sprintf("repeated replay journal digests differ: %s != %s",
				journal.Digest(first.report.Journal), journal.Digest(second.report.Journal)),
		})
	}
	if !bytes.Equal(first.outcome.Payload, second.outcome.Payload) ||
		!bytes.Equal(first.outcome.Transcript, second.outcome.Transcript) {
		findings = append(findings, apiv1.WorkflowGymReplayFinding{
			Code:   "harness_output_mismatch",
			Detail: "repeated replay produced different completion or transcript bytes",
		})
	}
	if !slices.Equal(firstEffects, secondEffects) {
		findings = append(findings, apiv1.WorkflowGymReplayFinding{
			Code:   "effect_ledger_mismatch",
			Detail: "repeated replay observed different effects through the sandbox",
		})
	}
	return findings
}

type replayResult struct {
	outcome harness.Outcome
	report  harness.BenchmarkReport
}

func runReplay(ctx context.Context, req ReplayRequest, fixture harness.BenchmarkFixture) (result replayResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return replayResult{}, err
	}
	workspace, err := os.MkdirTemp("", "goobers-workflow-gym-replay-*")
	if err != nil {
		return replayResult{}, fmt.Errorf("workflow gym replay: create workspace: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(workspace); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("workflow gym replay: remove workspace: %w", err))
			result = replayResult{}
		}
	}()

	sandbox, err := harness.NewBenchmarkSandbox(fixture)
	if err != nil {
		return replayResult{}, err
	}
	envelope := apiv1.InvocationEnvelope{
		TaskID:     req.Task.TaskID,
		WorkflowID: "workflow-gym-replay",
		RunID:      req.Task.TaskID,
		Gaggle:     "workflow-gym",
		Goal:       req.Task.ExpectedOutcome,
		Workspace:  workspace,
		RepoRef: apiv1.RepoRef{
			Provider: apiv1.Provider(fixture.Provider.Provider),
			Owner:    fixture.Provider.Repository.Owner,
			Name:     fixture.Provider.Repository.Name,
		},
	}
	outcome, _ := sandbox.Adapter(req.Act).Run(ctx, harness.RunRequest{
		Envelope: envelope, Workspace: workspace, CompletionPath: harness.DefaultResultPath,
	})
	report, err := sandbox.Report()
	if err != nil {
		return replayResult{}, err
	}
	return replayResult{outcome: outcome, report: report}, nil
}
