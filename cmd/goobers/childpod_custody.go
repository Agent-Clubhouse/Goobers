package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

const childPodWriterStarted = "isolated.child.writer.started"
const childPodWriterJoined = "isolated.child.writer.joined"

type childPodScope struct {
	Event    journal.Event
	Contract childpod.Contract
	Retained childpod.RetainedAttempt
}

type childInvocationBlobs struct {
	childpod.ScopedBlobs
	recorder    runner.OwnedJournalRecorder
	contract    childpod.Contract
	digest      string
	started     bool
	retainedRef journal.Ref
}

func (b *childInvocationBlobs) BindContract(ctx context.Context, digest string) error {
	if b.retainedRef.Digest == "" {
		return errors.New("child dispatch retention unavailable")
	}
	if err := b.ScopedBlobs.BindContract(ctx, digest); err != nil {
		return err
	}
	data, err := b.Get(ctx, digest)
	if err != nil {
		return err
	}
	b.contract, err = childpod.DecodeContract(data, digest)
	if err != nil {
		return err
	}
	b.digest = digest
	if err := b.record(childPodWriterStarted); err != nil {
		return err
	}
	b.started = true
	return nil
}
func (b *childInvocationBlobs) record(kind string) error {
	return b.recorder.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: b.contract.Stage, Attempt: b.contract.Attempt, Runner: map[string]any{"kind": kind, "contractDigest": b.digest, "retainedAttempt": b.retainedRef}})
}

func (b *childInvocationBlobs) keepAttempt(ctx context.Context, retained childpod.RetainedAttempt) error {
	data, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	if len(data) > childpod.MaxRetainedAttemptBytes {
		return errors.New("child attempt exceeds retention bound")
	}
	if err := b.Put(ctx, journal.Digest(data), data); err != nil {
		return err
	}
	b.retainedRef, err = childpod.RecordRetainedAttempt(b.recorder, retained)
	return err
}

// Terminal stage events cannot close a physical pod scope. These host-only
// markers bind custody to the original signed contract, never a pod report.
func (s *daemonCredentialService) childPodCustodyPending(ctx context.Context, reader *journal.Reader, digest string) (bool, error) {
	pending, events, err := s.pendingChildPodScopes(ctx, reader)
	if err != nil {
		return false, err
	}
	scope, ok := pending[digest]
	if !ok {
		return false, nil
	}
	for _, event := range events {
		if (event.Type == journal.EventStageStarted || event.Type == journal.EventReviewerStarted) && event.Stage == scope.Event.Stage && event.Branch == scope.Event.Branch && event.Seq > uint64(scope.Contract.PodAttempt) {
			return false, invoke.ErrWorkspaceNotQuiescent
		}
	}
	return true, nil
}

func (s *daemonCredentialService) pendingChildPodScopes(ctx context.Context, reader *journal.Reader) (map[string]childPodScope, []journal.Event, error) {
	id, err := reader.Identity()
	if err != nil {
		return nil, nil, err
	}
	if id.Child == nil || s.childQueue == nil {
		return nil, nil, invoke.ErrWorkspaceNotQuiescent
	}
	child, err := s.childQueue.ChildForExecutionRun(ctx, id.RunID)
	if err != nil {
		return nil, nil, err
	}
	store := childpod.ScopedBlobs{Queue: s.childQueue, Identity: child.Identity}
	events, err := reader.Events()
	if err != nil {
		return nil, nil, err
	}
	pending := map[string]childPodScope{}
	seen := map[string]bool{}
	physical := map[int]bool{}
	for _, event := range events {
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != childPodWriterStarted && kind != childPodWriterJoined) {
			continue
		}
		digest, ok := event.Runner["contractDigest"].(string)
		if !ok || !blobstore.ValidDigest(digest) {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == childPodWriterStarted {
			contract, err := readChildPodContract(ctx, store, reader, id, events, event, digest)
			if err != nil || seen[digest] || physical[contract.PodAttempt] {
				return nil, nil, errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
			}
			seen[digest], physical[contract.PodAttempt] = true, true
			retained, err := readChildRetainedAttempt(ctx, reader, store, event, contract, digest)
			if err != nil {
				return nil, nil, err
			}
			pending[digest] = childPodScope{Event: event, Contract: contract, Retained: retained}
			continue
		}
		scope, ok := pending[digest]
		if !ok || scope.Event.Stage != event.Stage || scope.Event.Attempt != event.Attempt || scope.Event.Branch != event.Branch {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		delete(pending, digest)
	}
	return pending, events, nil
}

func readChildPodContract(ctx context.Context, store childpod.ScopedBlobs, reader *journal.Reader, id journal.RunIdentity, events []journal.Event, event journal.Event, digest string) (childpod.Contract, error) {
	data, err := store.Get(ctx, digest)
	if err != nil {
		return childpod.Contract{}, err
	}
	contract, err := childpod.DecodeContract(data, digest)
	if err != nil || contract.Identity.Child == nil || !reflect.DeepEqual(contract.Identity, id) || contract.Stage != event.Stage || contract.Attempt != event.Attempt {
		return contract, errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
	}
	review, err := childContractReview(reader, contract)
	if err != nil {
		return contract, err
	}
	kind := journal.EventStageStarted
	if review {
		kind = journal.EventReviewerStarted
	}
	for _, started := range events {
		if started.Seq != uint64(contract.PodAttempt) {
			continue
		}
		if started.Type == kind && started.Stage == event.Stage && started.Attempt == event.Attempt && started.Branch == contract.ChildBranch && event.Branch == contract.ChildBranch && started.Time.Equal(contract.StartedAt) && event.Seq > started.Seq {
			return contract, nil
		}
		break
	}
	return contract, fmt.Errorf("%w: generated contract has no exact physical origin", invoke.ErrWorkspaceNotQuiescent)
}

func (s *daemonCredentialService) reconcileChildPodCustody(ctx context.Context, reader *journal.Reader) error {
	pending, _, err := s.pendingChildPodScopes(ctx, reader)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if len(pending) > 128 || s.childPodRecovery == nil {
		return fmt.Errorf("%w: generated worker custody requires exact reconciliation", invoke.ErrWorkspaceNotQuiescent)
	}
	var digest string
	var scope childPodScope
	for key, value := range pending {
		if digest == "" || value.Event.Seq < scope.Event.Seq {
			digest, scope = key, value
		}
	}
	eligible, err := s.childPodCustodyPending(ctx, reader, digest)
	if err != nil || !eligible {
		return errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
	}
	if err := s.childPodRecovery(ctx, reader, digest, scope); err != nil {
		return err
	}
	if len(pending) > 1 {
		return invoke.ErrChildCustodyPending
	}
	return nil
}

func readChildRetainedAttempt(ctx context.Context, reader *journal.Reader, store childpod.ScopedBlobs, event journal.Event, contract childpod.Contract, digest string) (childpod.RetainedAttempt, error) {
	var ref journal.Ref
	raw, err := json.Marshal(event.Runner["retainedAttempt"])
	if err != nil || json.Unmarshal(raw, &ref) != nil || ref.Integrity != apiv1.IntegrityTrusted {
		return childpod.RetainedAttempt{}, invoke.ErrWorkspaceNotQuiescent
	}
	retained, err := childpod.ReadRetainedAttempt(reader, ref)
	if err != nil {
		return retained, err
	}
	exact, err := store.GetBounded(ctx, ref.Digest, childpod.MaxRetainedAttemptBytes)
	if err != nil {
		return retained, err
	}
	canonical, err := json.Marshal(retained)
	if err != nil || !bytes.Equal(exact, canonical) {
		return retained, invoke.ErrWorkspaceNotQuiescent
	}
	a := retained.Input.Attempt
	if a.RunID != contract.Identity.RunID || a.Gaggle != contract.Identity.Gaggle || a.Workflow != contract.Identity.Workflow || a.Stage != contract.Stage || a.Number != contract.Attempt || a.PodAttempt != contract.PodAttempt || a.ChildExecutionDigest != digest || (retained.HostSnapshot == nil) != (contract.Workspace == nil) {
		return retained, invoke.ErrWorkspaceNotQuiescent
	}
	return retained, nil
}

// Root stages and terminal settlement require the whole family to be quiet.
// Static sibling branches own independent scratch/read-only stage workspaces.
func childBranchHasPending(pending map[string]childPodScope, branch int) bool {
	for _, scope := range pending {
		if branch == 0 || scope.Event.Branch == 0 || scope.Event.Branch == branch {
			return true
		}
	}
	return false
}
