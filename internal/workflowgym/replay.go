package workflowgym

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
)

// ReplayRequest selects one task and its historical, read-only replay inputs.
// Act is optional: callers that evaluate candidate behavior provide a scripted
// benchmark act, while the CLI replays the recorded harness session itself.
type ReplayRequest struct {
	Task                apiv1.WorkflowGymTaskManifest
	ProviderFixturePath string
	JournalDir          string
	Act                 harness.BenchmarkAct
}

// Replay runs the historical task twice in fresh workspaces and returns a
// canonical manifest describing its offline effects and determinism.
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
		Status:        first.report.Status,
		JournalDigest: journal.Digest(first.report.Journal),
		Effects: []apiv1.WorkflowGymReplayEffect{{
			Disposition: apiv1.WorkflowGymEffectDeterministic,
			Effect:      "recorded-journal",
			Target:      filepath.Clean(req.JournalDir),
		}},
		VirtualizationGaps:     make([]apiv1.WorkflowGymReplayFinding, 0),
		NondeterminismFindings: make([]apiv1.WorkflowGymReplayFinding, 0),
	}
	for _, exchange := range fixture.Provider.Exchanges {
		manifest.Effects = append(manifest.Effects, apiv1.WorkflowGymReplayEffect{
			Disposition: apiv1.WorkflowGymEffectVirtualized,
			Effect:      "provider-exchange",
			Target:      exchange.Method + " " + exchange.Path,
			Detail:      exchange.Name,
		})
	}
	for _, violation := range first.report.Violations {
		manifest.Effects = append(manifest.Effects, apiv1.WorkflowGymReplayEffect{
			Disposition: apiv1.WorkflowGymEffectUnavailable,
			Effect:      string(violation.Effect),
			Target:      violation.Target,
			Detail:      violation.Message,
		})
		if violation.Code == harness.BenchmarkErrorVirtualizationGap {
			manifest.VirtualizationGaps = append(manifest.VirtualizationGaps, apiv1.WorkflowGymReplayFinding{
				Code: violation.Code, Detail: violation.Message,
			})
		}
	}
	if !bytes.Equal(first.report.Journal, second.report.Journal) {
		manifest.NondeterminismFindings = append(manifest.NondeterminismFindings, apiv1.WorkflowGymReplayFinding{
			Code: "journal_mismatch",
			Detail: fmt.Sprintf("repeated replay journal digests differ: %s != %s",
				journal.Digest(first.report.Journal), journal.Digest(second.report.Journal)),
		})
	}
	if !bytes.Equal(first.outcome.Payload, second.outcome.Payload) ||
		!bytes.Equal(first.outcome.Transcript, second.outcome.Transcript) {
		manifest.NondeterminismFindings = append(manifest.NondeterminismFindings, apiv1.WorkflowGymReplayFinding{
			Code:   "harness_output_mismatch",
			Detail: "repeated replay produced different completion or transcript bytes",
		})
	}
	return manifest, nil
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
