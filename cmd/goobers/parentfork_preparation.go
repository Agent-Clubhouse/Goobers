package main

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

type parentRetirementWork struct {
	forks        []runner.ParentForkRecovery
	joins        []spec.JoinState
	candidates   []runner.ParentRetirementCandidate
	preparations []parallelworkspace.PendingPreparation
}

func readParentRetirementWork(reader *journal.Reader) (parentRetirementWork, error) {
	var work parentRetirementWork
	var err error
	work.forks, err = runner.PendingParentForks(reader)
	if err != nil {
		return work, err
	}
	work.preparations, err = parallelworkspace.PendingPreparations(reader)
	if err != nil {
		return work, err
	}
	work.joins, err = spec.PendingJoins(reader)
	if err != nil {
		return work, err
	}
	work.candidates, err = retirementCandidatesBeforeForkRecovery(reader, work.forks)
	return work, err
}

func (w parentRetirementWork) empty() bool {
	return len(w.joins) == 0 && len(w.forks) == 0 && len(w.candidates) == 0 && len(w.preparations) == 0
}

func (r parentArchiveRestorer) releaseForkPreparations(ctx context.Context, run *journal.Run, reader *journal.Reader, at time.Time) error {
	pending, err := parallelworkspace.PendingPreparations(reader)
	if err != nil {
		return err
	}
	retired, err := runner.RetiredParentForks(reader)
	if err != nil {
		return err
	}
	for _, value := range pending {
		covered, err := preparedForkCovered(reader, value, retired)
		if err != nil {
			return err
		}
		if err := r.releaseForkPreparation(ctx, value, covered, at); err != nil {
			return err
		}
		if err := parallelworkspace.ReleasePreparation(run, value); err != nil {
			return err
		}
	}
	return nil
}

func preparedForkCovered(reader *journal.Reader, value parallelworkspace.PendingPreparation, retired []runner.ParentForkRetirement) (bool, error) {
	request := value.Value.Request
	for _, entry := range retired {
		plan := entry.Plan
		if plan.Sequence != request.Sequence || plan.Parallel != request.Parallel {
			continue
		}
		if request.Join {
			if request.Plan != entry.Reference || request.Seed != plan.Source || plan.Root == nil || request.Custody != *plan.Root {
				return false, errors.New("parallel merge preparation differs from archived root")
			}
			return true, parallelworkspace.AcknowledgedJoinPreparation(reader, value.Value)
		}
		if request.Branch == 0 {
			return value.Value.Snapshot.Record.SnapshotSHA == plan.Source.SnapshotSHA, nil
		}
		if request.Plan != entry.Reference || request.Seed != plan.Source || request.Branch > len(plan.Workspaces) || request.Custody != plan.Workspaces[request.Branch-1] {
			return false, errors.New("parallel preparation differs from archived fork plan")
		}
		return true, nil
	}
	if request.Branch != 0 || request.Join {
		return false, errors.New("parallel result preparation has no archived fork owner")
	}
	return false, nil
}

func (r parentArchiveRestorer) releaseForkPreparation(ctx context.Context, value parallelworkspace.PendingPreparation, covered bool, at time.Time) error {
	record := value.Value.Snapshot.Record
	project, err := recoveryConfiguredProject(r.config, record.RepositoryKey)
	if err != nil {
		return err
	}
	url, err := r.cloneURL(project)
	if err != nil {
		return err
	}
	if (value.Value.Request.Branch != 0 || value.Value.Request.Join) && value.Value.Request.Custody.RepositoryDigest != worktree.RepositoryDigest(url) {
		return errors.New("parallel preparation repository changed")
	}
	found, err := r.worktrees.WithExistingMirror(ctx, url, func(repository string) error {
		if !covered {
			return r.retainOrphanForkPreparation(ctx, repository, record, at)
		}
		retained, err := r.forkSourceRetained(ctx, record)
		if err != nil || retained {
			return err
		}
		return recovery.DeletePreparedSnapshotRef(ctx, repository, record)
	})
	if err == nil && !found {
		return errors.New("parallel preparation mirror unavailable")
	}
	return err
}

// A source captured before any fork plan may be the only retained copy. Move
// that exact snapshot into normal inventory; never discard it as scratch data.
func (r parentArchiveRestorer) retainOrphanForkPreparation(ctx context.Context, repository string, record recovery.Record, at time.Time) error {
	publication := recoveryCleanupJournal{directory: r.layout.SchedulerDir(), scrubber: r.scrubber}
	request, err := recoveryCleanupRequest(r.layout, r.config, r.worktrees.Root, r.worktrees, record.RepositoryKey, repository, record.RunID, at, publication)
	if err != nil {
		return err
	}
	if request.RetainUntil.After(record.RetainUntil) {
		record.RetainUntil = request.RetainUntil
	}
	request.IdentityTime = record.CreatedAt
	request.RetainUntil = record.RetainUntil
	request.BaseRef = record.BaseRef
	request.SkipEmpty = false
	_, _, err = recovery.RetainPrepared(ctx, request, record, publication)
	return err
}
