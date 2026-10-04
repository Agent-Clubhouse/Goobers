package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// ParentContributionKind is a host-only receipt for a verified imported tree.
const ParentContributionKind = "isolated.parent.contribution"

// ParentContributionRetiredKind records a durable retirement authorization,
// after archive verification and before checkout removal. Cleanup is idempotent;
// the referenced output remains readable under ordinary journal retention.
const ParentContributionRetiredKind = "isolated.parent.contribution.retired"

const maxParentContributionBytes = 24 << 20

type parentContribution struct {
	Version        int                             `json:"version"`
	ContractDigest string                          `json:"contractDigest"`
	Custody        ContainedParentWorkspaceCustody `json:"custody"`
	Output         journal.Ref                     `json:"output"`
}

// RecordParentContribution is called only after stopped pod output has been
// verified and imported. It binds the exact artifact ref to the held checkout.
func RecordParentContribution(rec OwnedJournalRecorder, env apiv1.InvocationEnvelope, contract string, output journal.Ref) error {
	_, branch, err := OwnedJournalScope(rec)
	if err != nil || env.ChildWorkflowOrigin == nil {
		return errors.Join(errors.New("parent contribution lacks owned origin"), err)
	}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	if _, err := reader.ArtifactBytesBounded(output, maxParentContributionBytes); err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	started, custody, err := parentContributionCustody(events, env, branch)
	if err != nil {
		return err
	}
	if !blobstore.ValidDigest(contract) {
		return errors.New("invalid parent contribution contract")
	}
	record := parentContribution{Version: 1, ContractDigest: contract, Custody: custody, Output: output}
	exists, err := parentContributionReplay(events, record)
	if err != nil || exists {
		return err
	}
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: started.Stage, Attempt: started.Attempt, Branch: branch, Runner: map[string]any{"kind": ParentContributionKind, "contractDigest": contract, "contribution": record}})
}

func parentContributionCustody(events []journal.Event, env apiv1.InvocationEnvelope, branch int) (journal.Event, ContainedParentWorkspaceCustody, error) {
	var custody ContainedParentWorkspaceCustody
	var started journal.Event
	for _, event := range events {
		if event.Branch != branch {
			continue
		}
		if event.Type == journal.EventStageStarted {
			origin, err := journal.ChildWorkflowOriginForEvent(env.RunID, event)
			if err == nil && *origin == *env.ChildWorkflowOrigin {
				started = event
				custody = ContainedParentWorkspaceCustody{}
			}
		}
		if started.Seq != 0 && event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == ContainedParentWorkspaceKind && event.Stage == started.Stage && event.Attempt == started.Attempt {
			raw, _ := json.Marshal(event.Runner["custody"])
			if json.Unmarshal(raw, &custody) != nil {
				return started, custody, errors.New("invalid parent workspace custody")
			}
		}
	}
	if started.Seq == 0 || custody.Origin == nil || *custody.Origin != *env.ChildWorkflowOrigin || custody.Workspace.OwnerRunID != env.RunID {
		return started, custody, errors.New("parent contribution differs from held checkout")
	}
	return started, custody, nil
}

func parentContributionReplay(events []journal.Event, record parentContribution) (bool, error) {
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionKind || event.Runner["contractDigest"] != record.ContractDigest {
			continue
		}
		previous, err := decodeParentContribution(event)
		if err != nil || !reflect.DeepEqual(previous, record) {
			return false, errors.Join(errors.New("parent contribution changed on replay"), err)
		}
		return true, nil
	}
	return false, nil
}

func decodeParentContribution(event journal.Event) (parentContribution, error) {
	var result parentContribution
	raw, err := json.Marshal(event.Runner["contribution"])
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &result) != nil || result.Version != 1 || !blobstore.ValidDigest(result.ContractDigest) || result.ContractDigest != event.Runner["contractDigest"] || result.Custody.Version != 1 || result.Custody.Origin == nil || result.Output.Path == "" || result.Output.Size <= 0 || result.Output.Size > maxParentContributionBytes {
		return result, errors.New("invalid parent contribution receipt")
	}
	return result, nil
}

func latestParentContributions(events []journal.Event) (map[string]journal.Event, error) {
	latest := map[string]journal.Event{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionKind {
			continue
		}
		value, err := decodeParentContribution(event)
		if err != nil {
			return nil, err
		}
		latest[value.Custody.Workspace.WorkspaceID] = event
		if len(latest) > maxParentForks {
			return nil, errors.New("parent contribution exceeds workspace bound")
		}
	}
	return latest, nil
}

func (r *Runner) restoreParentContribution(ctx context.Context, tf *taskFrame, branch int) error {
	if tf.t.ChildWorkflows == nil || r.cfg.ChildWorkflowAdmission == nil || tf.heldChildWorkspace != nil {
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
	event, fork, err := selectParentContribution(events, tf.t, branch)
	if err != nil {
		return err
	}
	if fork {
		return r.forkParentContribution(ctx, tf, reader, events, event, branch)
	}
	if event.Seq == 0 {
		return nil
	}
	contribution, err := decodeParentContribution(event)
	if err != nil {
		return err
	}
	if contribution.Custody.Workspace.OwnerRunID != tf.in.RunID {
		return errors.New("parent contribution belongs to another run")
	}
	if _, err := reader.ArtifactBytesBounded(contribution.Output, maxParentContributionBytes); err != nil {
		return err
	}
	url, err := r.cfg.RepoCloneURL(tf.in.RepoRef)
	if err != nil {
		return err
	}
	wt, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, contribution.Custody.Workspace)
	if err != nil {
		return err
	}
	tf.heldChildWorkspace = &stageWorkspace{path: wt.Path, worktree: wt, retainedChild: func(context.Context) error { return nil }, parentContribution: true}
	return nil
}

func (r *Runner) retireContainedParentContributions(runID string, phase journal.RunPhase, writer *journal.Run) error {
	if phase != journal.PhaseCompleted || r.cfg.ChildWorkflowAdmission == nil {
		return nil
	}
	if err := r.verifyChildWorkflowCustody(writer.Dir()); err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	latest, err := latestParentContributions(events)
	if err != nil {
		return err
	}
	if err := parentContributionCoverage(events, latest); err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if len(latest) > 0 && id.WorkspaceRepository == nil {
		return errors.New("parent contribution lacks pinned repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, event := range latest {
		contribution, err := decodeParentContribution(event)
		if err != nil {
			return err
		}
		if contribution.Custody.Workspace.OwnerRunID != runID {
			return errors.New("parent archive owner changed")
		}
		if _, err := reader.ArtifactBytesBounded(contribution.Output, maxParentContributionBytes); err != nil {
			return fmt.Errorf("retain parent contribution: %w", err)
		}
		url, err := r.cfg.RepoCloneURL(*id.WorkspaceRepository)
		if err != nil {
			return err
		}
		if _, err := writer.AppendIfAbsent(journal.Event{Type: journal.EventRunnerAnnotation, Stage: event.Stage, Attempt: event.Attempt, Branch: event.Branch, Runner: map[string]any{"kind": ParentContributionRetiredKind, "contractDigest": contribution.ContractDigest, "contribution": contribution}}, func(old journal.Event) bool {
			return old.Type == journal.EventRunnerAnnotation && old.Runner["kind"] == ParentContributionRetiredKind && old.Runner["contractDigest"] == contribution.ContractDigest
		}); err != nil {
			return err
		}
		if err := r.cfg.Worktrees.RetireHeldStage(ctx, url, contribution.Custody.Workspace); err != nil {
			return err
		}
	}
	return nil
}

// PendingParentContributions pins the journal while any verified imported
// checkout lacks durable retirement authorization. Malformed receipts refuse
// pruning. The artifact remains ordinary journal evidence after authorization.
func PendingParentContributions(events []journal.Event) (bool, error) {
	latest, err := latestParentContributions(events)
	if err != nil {
		return false, err
	}
	if err := parentContributionCoverage(events, latest); err != nil {
		return true, err
	}
	for _, contribution := range latest {
		authorized, err := parentRetirementAuthorized(events, contribution)
		if err != nil {
			return false, err
		}
		if !authorized {
			return true, nil
		}
	}
	return false, nil
}

func parentRetirementAuthorized(events []journal.Event, contribution journal.Event) (bool, error) {
	for _, event := range events {
		if event.Seq <= contribution.Seq || event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionRetiredKind || event.Runner["contractDigest"] != contribution.Runner["contractDigest"] {
			continue
		}
		old, err := decodeParentContribution(contribution)
		retired, decodeErr := decodeParentContribution(event)
		if err != nil || decodeErr != nil || !reflect.DeepEqual(old, retired) {
			return false, errors.New("parent retirement receipt changed")
		}
		return true, nil
	}
	return false, nil
}

func parentContributionCoverage(events []journal.Event, latest map[string]journal.Event) error {
	holds, err := parentWorkspaceHolds(events)
	if err != nil {
		return err
	}
	for id, hold := range holds {
		contribution, found := latest[id]
		if !found || contribution.Seq <= hold.Seq {
			return errors.New("parent workspace has no final retained contribution")
		}
		value, err := decodeParentContribution(contribution)
		if err != nil {
			return err
		}
		if hold.Runner["kind"] == ParentForkReadyKind || hold.Runner["kind"] == ParentForkPlannedKind {
			continue
		}
		data, _ := json.Marshal(hold.Runner["custody"])
		var custody ContainedParentWorkspaceCustody
		if json.Unmarshal(data, &custody) != nil || !reflect.DeepEqual(custody, value.Custody) {
			return errors.New("parent contribution differs from final workspace custody")
		}
	}
	return nil
}

// A startup finalizer may replay already-authorized removal without opening a
// second journal writer. It cannot authorize a new retirement or discard work
// whose archive/receipt is missing. Resume owns that repair path.
func (r *Runner) replayContainedParentRetirements(runID string, phase journal.RunPhase) error {
	if phase != journal.PhaseCompleted || r.cfg.ChildWorkflowAdmission == nil {
		return nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(r.cfg.RunsDir, runID))
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	latest, err := latestParentContributions(events)
	if err != nil || len(latest) == 0 {
		return err
	}
	if err := parentContributionCoverage(events, latest); err != nil {
		return err
	}
	if err := r.verifyChildWorkflowCustody(reader.Dir()); err != nil {
		return err
	}
	id, err := reader.Identity()
	if err != nil {
		return err
	}
	if id.RunID != runID || id.WorkspaceRepository == nil {
		return errors.New("parent retirement has no pinned owner repository")
	}
	url, err := r.cfg.RepoCloneURL(*id.WorkspaceRepository)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, event := range latest {
		authorized, err := parentRetirementAuthorized(events, event)
		if err != nil {
			return err
		}
		if !authorized {
			continue
		}
		contribution, err := decodeParentContribution(event)
		if err != nil {
			return err
		}
		if _, err := reader.ArtifactBytesBounded(contribution.Output, maxParentContributionBytes); err != nil {
			return err
		}
		if err := r.cfg.Worktrees.RetireHeldStage(ctx, url, contribution.Custody.Workspace); err != nil {
			return err
		}
	}
	return nil
}

// RequireRetiredParentContributions is the journal-retention boundary: malformed
// custody, unresolved work, or unreadable archived bytes prevent deletion.
func RequireRetiredParentContributions(reader *journal.Reader) error {
	events, err := reader.Events()
	if err != nil {
		return err
	}
	pending, err := PendingParentContributions(events)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("parent contribution custody has not been retired")
	}
	latest, err := latestParentContributions(events)
	if err != nil {
		return err
	}
	for _, event := range latest {
		value, err := decodeParentContribution(event)
		if err != nil {
			return err
		}
		if _, err := reader.ArtifactBytesBounded(value.Output, maxParentContributionBytes); err != nil {
			return err
		}
	}
	return nil
}
