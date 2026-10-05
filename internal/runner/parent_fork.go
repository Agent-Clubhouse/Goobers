package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// Host-only fork markers pin custody before checkout creation and certify
// readiness after exact managed ownership is established.
const (
	ParentForkPlannedKind = "isolated.parent.fork.planned"
	ParentForkReadyKind   = "isolated.parent.fork.ready"
	maxParentForks        = 512
)

// Reserve the entire declared fan-out before launching branch goroutines. A
// check performed independently by each writer would race at the hard bound.
func parentParallelForkBudget(runID string, count int, events []journal.Event) error {
	holds, err := parentWorkspaceHolds(events)
	if err != nil {
		return err
	}
	sequence := parentParallelBoundary(events, 1)
	needed := len(holds)
	for branch := 1; branch <= count; branch++ {
		if _, exists := holds[fmt.Sprintf("%s-p%d-b%d", runID, sequence, branch)]; !exists {
			needed++
		}
	}
	if needed > maxParentForks {
		return errors.New("parent workspace fork limit reached before fan-out")
	}
	return nil
}

// A fork plan is host-only, bounded custody. The original producer's immutable
// artifact is its input; a sibling's live checkout is never read or reset.
type parentFork struct {
	Version          int                   `json:"version"`
	ParallelSequence uint64                `json:"parallelSequence"`
	SourceSequence   uint64                `json:"sourceSequence"`
	SourceContract   string                `json:"sourceContract"`
	Workspace        worktree.StageCustody `json:"workspace"`
}

func (r *Runner) forkParentContribution(ctx context.Context, tf *taskFrame, reader *journal.Reader, events []journal.Event, source journal.Event, branch int) error {
	if source.Seq == 0 {
		return errors.New("contained parallel requires a completed repository seed before fan-out")
	}
	contribution, err := decodeParentContribution(source)
	if err != nil {
		return err
	}
	data, err := reader.ArtifactBytesBounded(contribution.Output, maxParentContributionBytes)
	if err != nil {
		return err
	}
	returned, err := recovery.ReadPortableReturn(data, contribution.Output.Digest, contribution.ContractDigest)
	if err != nil {
		return err
	}
	url, err := r.cfg.RepoCloneURL(tf.in.RepoRef)
	if err != nil {
		return err
	}
	if contribution.Custody.Workspace.OwnerRunID != tf.in.RunID || contribution.Custody.Workspace.RepositoryDigest != worktree.RepositoryDigest(url) {
		return errors.New("parent fork source belongs to another repository or run")
	}
	sequence := parentParallelBoundary(events, branch)
	forkID := fmt.Sprintf("p%d-b%d", sequence, branch)
	opts := worktree.ChildOptions{RepoURL: url, RunID: tf.in.RunID + "-" + forkID, OwnerRunID: tf.in.RunID, Gaggle: tf.in.Gaggle, ForkID: forkID}
	err = r.cfg.Worktrees.WithRecoveryMirror(ctx, url, func(mirror string) error {
		prepared, prepareErr := recovery.PreparePortableFork(ctx, mirror, contribution.Custody.Workspace.StartRef, opts.RunID, *returned.Workspace, source.Time)
		opts.SnapshotSHA = prepared.SnapshotSHA
		return prepareErr
	})
	if err != nil {
		return err
	}
	plan := parentFork{Version: 1, ParallelSequence: sequence, SourceSequence: source.Seq, SourceContract: contribution.ContractDigest, Workspace: worktree.StageCustody{WorkspaceID: opts.RunID, OwnerRunID: opts.OwnerRunID, RepositoryDigest: worktree.RepositoryDigest(url), Branch: "goobers/parents/" + tf.in.RunID + "/" + forkID, StartRef: opts.SnapshotSHA}}
	planned, ready, err := parentForkState(events, plan)
	if err != nil {
		return err
	}
	if !planned {
		holds, err := parentWorkspaceHolds(events)
		if err != nil {
			return err
		}
		if len(holds) >= maxParentForks {
			return errors.New("parent workspace fork limit reached")
		}
		if err = appendParentFork(tf.jr, tf.t.Name, ParentForkPlannedKind, plan); err != nil {
			return err
		}
	}
	workspace, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, plan.Workspace)
	if err != nil && !ready {
		workspace, err = r.createParentFork(ctx, opts)
	}
	if err != nil {
		return err
	}
	if !ready {
		if err = appendParentFork(tf.jr, tf.t.Name, ParentForkReadyKind, plan); err != nil {
			return err
		}
	}
	tf.heldChildWorkspace = &stageWorkspace{path: workspace.Path, worktree: workspace, retainedChild: func(context.Context) error { return nil }, parentContribution: true}
	return nil
}

func (r *Runner) createParentFork(ctx context.Context, opts worktree.ChildOptions) (*worktree.Worktree, error) {
	workspace, err := r.cfg.Worktrees.CreateChildFromSnapshot(ctx, opts)
	if err != nil {
		return nil, err
	}
	if _, err = workspace.HoldForChild(ctx); err != nil {
		return nil, err
	}
	return workspace, nil
}

func appendParentFork(rec executionJournal, stage, kind string, plan parentFork) error {
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Runner: map[string]any{"kind": kind, "fork": plan}})
}

func decodeParentFork(event journal.Event) (parentFork, error) {
	var value parentFork
	data, err := json.Marshal(event.Runner["fork"])
	if err != nil || len(data) > 8192 || json.Unmarshal(data, &value) != nil || value.Version != 1 || value.ParallelSequence == 0 || value.SourceSequence == 0 || value.Workspace.WorkspaceID == "" || value.Workspace.OwnerRunID == "" || value.Workspace.StartRef == "" || value.SourceContract == "" {
		return value, errors.New("invalid parent fork custody")
	}
	return value, nil
}

func parentForkState(events []journal.Event, expected parentFork) (bool, bool, error) {
	var planned, ready bool
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || (event.Runner["kind"] != ParentForkPlannedKind && event.Runner["kind"] != ParentForkReadyKind) {
			continue
		}
		value, err := decodeParentFork(event)
		if err != nil {
			return false, false, err
		}
		if value.Workspace.WorkspaceID != expected.Workspace.WorkspaceID {
			continue
		}
		if !reflect.DeepEqual(value, expected) {
			return false, false, errors.New("parent fork source changed on replay")
		}
		if event.Runner["kind"] == ParentForkReadyKind {
			ready = true
		} else {
			planned = true
		}
	}
	if ready && !planned {
		return false, false, errors.New("parent fork has no durable plan")
	}
	return planned, ready, nil
}

func parentWorkspaceHolds(events []journal.Event) (map[string]journal.Event, error) {
	holds := map[string]journal.Event{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		var custody worktree.StageCustody
		switch event.Runner["kind"] {
		case ParentForkPlannedKind, ParentForkReadyKind:
			plan, err := decodeParentFork(event)
			if err != nil {
				return nil, err
			}
			custody = plan.Workspace
		case ContainedParentWorkspaceKind:
			data, _ := json.Marshal(event.Runner["custody"])
			var value ContainedParentWorkspaceCustody
			if json.Unmarshal(data, &value) != nil || value.Version != 1 || value.Origin == nil || value.Workspace.WorkspaceID == "" {
				return nil, errors.New("invalid parent workspace custody")
			}
			custody = value.Workspace
		default:
			continue
		}
		holds[custody.WorkspaceID] = event
		if len(holds) > maxParentForks {
			return nil, errors.New("parent custody exceeds workspace bound")
		}
	}
	return holds, nil
}
