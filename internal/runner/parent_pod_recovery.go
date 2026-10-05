package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ContainedParentRecoveredKind is host-only acknowledged workspace recovery.
// It transfers retained working changes into the next same-stage attempt.
const ContainedParentRecoveredKind = "isolated.parent.recovered"
const maxParentRecoveryBytes = 256 << 10

type containedParentRecovery struct {
	Version             int                             `json:"version"`
	ContractDigest      string                          `json:"contractDigest"`
	Custody             ContainedParentWorkspaceCustody `json:"custody"`
	Context             []apiv1.ContextPointer          `json:"context,omitempty"`
	Transcript          *apiv1.ArtifactPointer          `json:"transcript,omitempty"`
	Usage               map[string]float64              `json:"usage,omitempty"`
	InstructionAddendum string                          `json:"instructionAddendum,omitempty"`
}

// RecordContainedParentRecovery preserves original context and supervised usage
// after verified tree import. Repeated recovery must reproduce the same receipt.
func RecordContainedParentRecovery(writer *journal.Run, scope journal.Event, digest string, custody ContainedParentWorkspaceCustody, env apiv1.InvocationEnvelope, transcript *apiv1.ArtifactPointer, usage map[string]float64) error {
	record := containedParentRecovery{Version: 1, ContractDigest: digest, Custody: custody, Context: env.ContextPointers, Transcript: transcript, Usage: usage, InstructionAddendum: env.InstructionAddendum}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxParentRecoveryBytes || len(record.Context) > 128 || len(record.Usage) > 128 {
		return errors.New("parent recovery context exceeds bound")
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == ContainedParentRecoveredKind && event.Runner["contractDigest"] == digest {
			var ref journal.Ref
			raw, _ := json.Marshal(event.Runner["recovery"])
			if json.Unmarshal(raw, &ref) != nil || ref.Digest != journal.Digest(data) || event.Stage != scope.Stage || event.Attempt != scope.Attempt || event.Branch != scope.Branch {
				return errors.New("parent recovery receipt changed")
			}
			return nil
		}
	}
	ref, err := writer.RecordArtifactBoundedWithIntegrity(fmt.Sprintf("parent-recovery/%s-%d.json", scope.Stage, scope.Seq), data, apiv1.IntegrityTrusted, maxParentRecoveryBytes)
	if err != nil {
		return err
	}
	if ref.Digest != journal.Digest(data) {
		return errors.New("parent recovery context changed at journal boundary")
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: scope.Stage, Attempt: scope.Attempt, Branch: scope.Branch, Runner: map[string]any{"kind": ContainedParentRecoveredKind, "contractDigest": digest, "recovery": ref}})
}

func (r *Runner) restoreContainedParentWorkspace(ctx context.Context, tf *taskFrame, branch int) error {
	if r.cfg.ChildWorkflowRecoveryAdmission == nil || tf.t.ChildWorkflows == nil || tf.childWaitResume != nil {
		return nil
	}
	reader, err := journal.OpenReadOnly(tf.jr.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	var candidate *journal.Event
	for i := range events {
		event := &events[i]
		if event.Stage != tf.t.Name || event.Branch != branch {
			continue
		}
		if event.Type == journal.EventStageFinished {
			candidate = nil
		}
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == ContainedParentRecoveredKind {
			candidate = event
		}
	}
	if candidate == nil {
		return nil
	}
	record, err := readParentRecovery(reader, *candidate, tf.in.RunID)
	if err != nil {
		return err
	}
	if r.cfg.Worktrees == nil || r.cfg.RepoCloneURL == nil {
		return errors.New("parent workspace recovery unavailable")
	}
	url, err := r.cfg.RepoCloneURL(tf.in.RepoRef)
	if err != nil {
		return err
	}
	workspace, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, record.Custody.Workspace)
	if err != nil {
		return err
	}
	tf.heldChildWorkspace = &stageWorkspace{path: workspace.Path, worktree: workspace, retainedChild: func(context.Context) error { return nil }}
	tf.containedRecovery = &record
	tf.upstream = append([]apiv1.ContextPointer(nil), record.Context...)
	if record.Transcript != nil {
		tf.upstream = append(tf.upstream, apiv1.ContextPointer{Name: "recovered-parent-transcript", Artifact: record.Transcript, Integrity: record.Transcript.Integrity})
	}
	return nil
}

func readParentRecovery(reader *journal.Reader, event journal.Event, runID string) (containedParentRecovery, error) {
	var record containedParentRecovery
	var ref journal.Ref
	raw, _ := json.Marshal(event.Runner["recovery"])
	if json.Unmarshal(raw, &ref) != nil {
		return record, errors.New("invalid parent recovery ref")
	}
	data, err := reader.ArtifactBytesBounded(ref, maxParentRecoveryBytes)
	if err != nil {
		return record, err
	}
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ContractDigest != event.Runner["contractDigest"] || record.Custody.Version != 1 || record.Custody.Origin == nil || record.Custody.Workspace.OwnerRunID != runID || len(record.Context) > 128 || len(record.Usage) > 128 {
		return record, errors.New("invalid parent recovery custody")
	}
	return record, nil
}

func restoreContainedParentUsage(tf taskFrame, total *stageUsageTotals, instruction *string) {
	if tf.containedRecovery == nil {
		return
	}
	accumulateStageUsage(total, maps.Clone(tf.containedRecovery.Usage))
	if *instruction == "" {
		*instruction = tf.containedRecovery.InstructionAddendum
	}
}

func (r *Runner) prepareRecoveredTaskContext(ctx context.Context, tf *taskFrame, branch int, start *int32, class *journal.AttemptClass, accounting **resumeRetryAccounting) error {
	if err := r.restoreContainedParentWorkspace(ctx, tf, branch); err != nil {
		return err
	}
	if err := r.restoreAcceptedParentWait(ctx, tf, start, class, accounting); err != nil {
		return err
	}
	tf.upstream = apiv1.SelectContextPointers(tf.upstream, tf.t.ContextFrom)
	return admitTaskIntegrity(*tf)
}
