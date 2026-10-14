package parallelworkspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// Join records immutable preparation and an exact application plan before
// changing the root. Acknowledged joins are not reapplied over later stages.
// The caller owns the root journal and has joined every branch writer.
func (s Service) Join(ctx context.Context, rec Recorder, request spec.JoinRequest) error {
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	if err := validateJoinRequest(reader, request); err != nil {
		return err
	}
	states, err := spec.ReadJoins(reader)
	if err != nil {
		return err
	}
	state := states[request.Sequence]
	if state == nil {
		state, err = s.prepareJoin(ctx, rec, reader, request)
		if err != nil {
			return err
		}
	}
	intent, err := readJoinIntent(reader, *state)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(intent.Request, request) {
		return errors.New("parallel join request changed after durable preparation")
	}
	if state.Ready == (journal.Ref{}) {
		if err := s.prepareJoinApplication(ctx, rec, reader, state, intent); err != nil {
			return err
		}
	}
	ready, snapshot, err := readJoinReady(reader, *state, intent)
	if err != nil || state.Applied {
		return err
	}
	workspace, url, err := s.joinRoot(ctx, rec, request)
	if err != nil {
		return err
	}
	if err := s.importSource(ctx, reader, url, ready.Source, snapshot); err != nil {
		return err
	}
	if err := recovery.ApplyChildApplication(ctx, workspace.Path, ready.Plan); err != nil {
		return err
	}
	return joinTransition(rec, *state, "applied")
}

func (s Service) joinRoot(ctx context.Context, rec Recorder, request spec.JoinRequest) (*worktree.Worktree, string, error) {
	if s.Worktrees == nil || s.CloneURL == nil {
		return nil, "", errors.New("parallel join root service unavailable")
	}
	url, err := s.CloneURL(request.Repository)
	if err != nil {
		return nil, "", err
	}
	if request.Root.OwnerRunID != request.RunID || request.Root.RepositoryDigest != worktree.RepositoryDigest(url) {
		return nil, "", errors.New("parallel join root physical owner changed")
	}
	if _, err := s.Prepare(ctx, rec, request.Request, &request.Seed); err != nil {
		return nil, "", err
	}
	workspace, err := s.Worktrees.AdoptHeldStage(ctx, url, request.Root)
	return workspace, url, err
}

func (s Service) buildJoin(ctx context.Context, rec Recorder, reader *journal.Reader, request spec.JoinRequest) (recovery.PreparedChildDisposition, *worktree.Worktree, error) {
	var empty recovery.PreparedChildDisposition
	workspace, _, err := s.joinRoot(ctx, rec, request)
	if err != nil {
		return empty, nil, err
	}
	seed, err := readSource(reader, request.Request, request.Seed)
	if err != nil {
		return empty, nil, err
	}
	var results []recovery.Record
	for _, result := range request.Results {
		if _, err := s.Result(ctx, rec, joinResultRequest(request, result), &result.Source); err != nil {
			return empty, nil, err
		}
		if result.Status != journal.BranchSucceeded {
			continue
		}
		snapshot, err := ReadResult(reader, joinResultRequest(request, result), result.Source)
		if err != nil {
			return empty, nil, err
		}
		results = append(results, snapshot.Record)
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d", request.RunID, request.Gaggle, request.Sequence)))
	prepared, err := recovery.PrepareChildFanIn(ctx, workspace.Path, seed, seed, results, fmt.Sprintf("parallel-join-%x", identity), request.At, MaxBundleBytes)
	if err != nil {
		return empty, nil, err
	}
	// The journal, rather than a run-only pin, owns the exact operation. Reuse
	// normal snapshot ownership and retention for every prepared join object.
	prepared.Prepared.RunID = request.RunID
	prepared.Prepared.BaseRef = prepared.Prepared.BaseSHA
	prepared.Prepared.Ref, err = recovery.RefForSnapshot(request.RunID, prepared.Prepared.SnapshotSHA)
	return prepared, workspace, err
}

func (s Service) prepareJoin(ctx context.Context, rec Recorder, reader *journal.Reader, request spec.JoinRequest) (*spec.JoinState, error) {
	prepared, _, err := s.buildJoin(ctx, rec, reader, request)
	if err != nil {
		return nil, fmt.Errorf("parallel join could not combine immutable results; all branch results retained: %w", err)
	}
	intent := joinIntent{Version: 1, Request: request, Prepared: prepared}
	ref, err := joinArtifact(rec, "parallel-join-intent.json", intent)
	if err != nil {
		return nil, err
	}
	state := &spec.JoinState{JoinReceipt: spec.JoinReceipt{Sequence: request.Sequence, Intent: ref}, Parallel: request.Parallel}
	if err := joinTransition(rec, *state, "prepared"); err != nil {
		return nil, err
	}
	return state, nil
}

func (s Service) prepareJoinApplication(ctx context.Context, rec Recorder, reader *journal.Reader, state *spec.JoinState, intent joinIntent) error {
	prepared, workspace, err := s.buildJoin(ctx, rec, reader, intent.Request)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(prepared, intent.Prepared) {
		return errors.New("parallel join preparation changed on replay")
	}
	plan, err := recovery.PlanChildApplication(ctx, workspace.Path, prepared)
	if err != nil {
		return err
	}
	request := intent.Request
	owner := spec.ResultRequest{Request: request.Request, Plan: request.Plan, Seed: request.Seed, Custody: request.Root, Join: true}
	source, err := recordSnapshot(ctx, rec, workspace.Path, "parallel-join", owner, joinSnapshot(prepared), func(snapshot recovery.ChildSnapshot) any {
		return joinCarrier{Version: 1, Intent: state.Intent, Snapshot: snapshot}
	})
	if err != nil {
		return err
	}
	ref, err := joinArtifact(rec, "parallel-join-application.json", joinReady{Version: 1, Intent: state.Intent, Source: source, Plan: plan})
	if err != nil {
		return err
	}
	state.Ready = ref
	return joinTransition(rec, *state, "ready")
}

// RecoverJoins runs under terminal journal ownership before root archival.
// It finishes only recorded intent, without asking a worker to run again.
func (s Service) RecoverJoins(ctx context.Context, rec Recorder) error {
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	states, err := spec.PendingJoins(reader)
	if err != nil {
		return err
	}
	for _, state := range states {
		intent, err := readJoinIntent(reader, state)
		if err != nil {
			return err
		}
		if err := s.Join(ctx, rec, intent.Request); err != nil {
			return err
		}
	}
	return nil
}
