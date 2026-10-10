package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

func readParentRecovery(reader *journal.Reader, event journal.Event, runID string) (containedParentRecovery, error) {
	var record containedParentRecovery
	var ref journal.Ref
	raw, _ := json.Marshal(event.Runner["recovery"])
	if json.Unmarshal(raw, &ref) != nil || ref.Integrity != apiv1.IntegrityTrusted {
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

func (r *Runner) restoreContainedParentWorkspace(ctx context.Context, tf *taskFrame, branch int) error {
	if tf.in.Child != nil || tf.t.ChildWorkflows == nil {
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
	record, found, err := selectParentRecovery(reader, events, tf.in.RunID, tf.t.Name, branch)
	if err != nil || !found {
		return err
	}
	if tf.childWaitResume != nil {
		old := tf.childWaitResume.Request.Origin
		if *record.Custody.Origin == old {
			return nil // the durable wait already carries this exact recovery
		}
		if tf.childWaitCompletion == nil || record.Custody.Origin.StageOccurrence != old.StageOccurrence {
			return errors.New("parent recovery cannot replace unresolved child custody")
		}
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
	tf.childWaitResume, tf.childWaitCompletion = nil, nil
	tf.upstream = append([]apiv1.ContextPointer(nil), record.Context...)
	if record.Transcript != nil {
		tf.upstream = append(tf.upstream, apiv1.ContextPointer{Name: "recovered-parent-transcript", Artifact: record.Transcript, Integrity: record.Transcript.Integrity})
	}
	return nil
}

func restoreContainedParentUsage(tf taskFrame, total *stageUsageTotals, instruction *string) error {
	if tf.containedRecovery == nil {
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
	var accounting *childAttemptAccounting
	origin := *tf.containedRecovery.Custody.Origin
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != childAttemptAccountingKind {
			continue
		}
		var header childAttemptAccounting
		data, _ := json.Marshal(event.Runner["accounting"])
		if json.Unmarshal(data, &header) != nil || header.Origin != origin {
			continue
		}
		accounting, err = readChildAttemptAccounting(event, origin, accounting != nil)
		if err != nil {
			return err
		}
	}
	if accounting == nil {
		return errors.New("parent recovery lacks original attempt accounting")
	}
	restored, err := restoreChildAccounting(*accounting, tf.containedRecovery.Usage)
	if err != nil {
		return err
	}
	*total = *restored
	if *instruction == "" {
		*instruction = tf.containedRecovery.InstructionAddendum
	}
	return nil
}

func (r *Runner) prepareRecoveredTaskContext(ctx context.Context, tf *taskFrame, branch int, start *int32, class *journal.AttemptClass, accounting **resumeRetryAccounting) error {
	if r.cfg.SelfExecutionDenied {
		if err := r.admitParentExecution(ctx, tf.in.Machine); err != nil {
			return err
		}
	}
	if err := r.restoreContainedParentWorkspace(ctx, tf, branch); err != nil {
		return err
	}
	if err := r.restoreAcceptedParentWait(ctx, tf, start, class, accounting); err != nil {
		return err
	}
	tf.upstream = apiv1.SelectContextPointers(tf.upstream, tf.t.ContextFrom)
	return admitTaskIntegrity(*tf)
}

// Recovery can only restore the latest physical owner of this stage/branch.
// A later start or finish consumes the earlier receipt; a late stale receipt
// is an error rather than authority to overwrite a replacement's checkout.
func selectParentRecovery(reader *journal.Reader, events []journal.Event, runID, stage string, branch int) (containedParentRecovery, bool, error) {
	var result containedParentRecovery
	var started journal.Event
	var candidate *journal.Event
	for i := range events {
		event := &events[i]
		if event.Branch != branch {
			continue
		}
		if event.Type == journal.EventStageStarted {
			started, candidate = *event, nil
		}
		if event.Stage != stage {
			continue
		}
		switch event.Type {
		case journal.EventStageFinished:
			candidate = nil
		case journal.EventRunnerAnnotation:
			if event.Runner["kind"] == ContainedParentRecoveredKind {
				candidate = event
			}
		}
	}
	if candidate == nil {
		return result, false, nil
	}
	result, err := readParentRecovery(reader, *candidate, runID)
	if err != nil {
		return result, false, err
	}
	origin, err := journal.ChildWorkflowOriginForEvent(runID, started)
	if err != nil || candidate.Attempt != started.Attempt || origin == nil || *origin != *result.Custody.Origin {
		return result, false, errors.New("parent recovery differs from latest stage owner")
	}
	return result, true, nil
}

func (r *Runner) restoreParentProgress(ctx context.Context, tf *taskFrame, total *stageUsageTotals, instruction *string) error {
	if err := restoreContainedParentUsage(*tf, total, instruction); err != nil {
		return err
	}
	if tf.childWaitResume == nil {
		return nil
	}
	if *instruction == "" {
		*instruction = tf.childWaitResume.InstructionAddendum
	}
	return r.restoreChildWait(ctx, tf, total)
}
