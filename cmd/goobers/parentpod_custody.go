package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

const parentPodWriterStarted = "isolated.parent.writer.started"
const parentPodWriterJoined = "isolated.parent.writer.joined"

func (b *parentInvocationBlobs) record(kind string) error {
	return b.recorder.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: b.contract.Stage, Attempt: b.contract.Attempt, Runner: map[string]any{"kind": kind, "contractDigest": b.contractDigest, "retainedAttempt": b.retainedRef}})
}

// Only the host can write these annotations. Pod journal ingress uses a strict
// observation allowlist. Unknown dispatched custody blocks journal replacement;
// neither terminal state nor a later attempt can acknowledge an older pod.
func verifyParentPodCustody(reader *journal.Reader) error {
	pending, _, err := pendingParentPodScopes(reader)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("%w: contained parent worker custody requires reconciliation before retry or resume", invoke.ErrWorkspaceNotQuiescent)
	}
	return nil
}

type parentPodScope struct {
	Event    journal.Event
	Contract childpod.Contract
}

// parentPodCustodyPending authorizes bounded late surrender for exactly one
// previously dispatched physical contract. It grants no execution, credentials,
// new child authority, or cross-attempt blob access.
func parentPodCustodyPending(reader *journal.Reader, contractDigest string) (bool, error) {
	if !blobstore.ValidDigest(contractDigest) {
		return false, invoke.ErrWorkspaceNotQuiescent
	}
	pending, events, err := pendingParentPodScopes(reader)
	if err != nil {
		return false, err
	}
	scope, ok := pending[contractDigest]
	if !ok {
		return false, nil
	}
	for _, event := range events {
		if event.Type == journal.EventStageStarted && event.Stage == scope.Event.Stage && event.Branch == scope.Event.Branch && event.Seq > uint64(scope.Contract.PodAttempt) {
			return false, invoke.ErrWorkspaceNotQuiescent
		}
	}
	return true, nil
}

func pendingParentPodScopes(reader *journal.Reader) (map[string]parentPodScope, []journal.Event, error) {
	id, err := reader.Identity()
	if err != nil {
		return nil, nil, err
	}
	if id.Child != nil {
		return nil, nil, nil
	}
	events, err := reader.Events()
	if err != nil {
		return nil, nil, err
	}
	store := childpod.ParentBlobs{RunDir: reader.Dir(), Identity: id}
	pending := map[string]parentPodScope{}
	seen := map[string]bool{}
	physical := map[int]bool{}
	for _, event := range events {
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != parentPodWriterStarted && kind != parentPodWriterJoined) {
			continue
		}
		digest, ok := event.Runner["contractDigest"].(string)
		if !ok || !blobstore.ValidDigest(digest) {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == parentPodWriterStarted {
			contract, err := readParentPodContract(store, events, event, digest)
			if err != nil || seen[digest] || physical[contract.PodAttempt] {
				return nil, nil, errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
			}
			seen[digest], physical[contract.PodAttempt] = true, true
			pending[digest] = parentPodScope{Event: event, Contract: contract}
			continue
		}
		started, ok := pending[digest]
		if !ok || started.Event.Stage != event.Stage || started.Event.Attempt != event.Attempt || started.Event.Branch != event.Branch {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		delete(pending, digest)
	}
	return pending, events, nil
}

func readParentPodContract(store childpod.ParentBlobs, events []journal.Event, event journal.Event, digest string) (childpod.Contract, error) {
	var contract childpod.Contract
	data, err := store.Get(context.Background(), digest)
	if err != nil {
		return contract, err
	}
	contract, err = childpod.DecodeContract(data, digest)
	if err != nil || contract.ParentOrigin == nil || !reflect.DeepEqual(contract.Identity, store.Identity) || contract.Stage != event.Stage || contract.Attempt != event.Attempt {
		return contract, errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
	}
	for _, start := range events {
		if start.Seq != uint64(contract.PodAttempt) {
			continue
		}
		origin, err := journal.ChildWorkflowOriginForEvent(store.Identity.RunID, start)
		if err == nil && reflect.DeepEqual(origin, contract.ParentOrigin) && start.Type == journal.EventStageStarted && start.Stage == event.Stage && start.Attempt == event.Attempt && start.Branch == event.Branch && start.Time.Equal(contract.StartedAt) && event.Seq > start.Seq {
			return contract, nil
		}
		break
	}
	return contract, invoke.ErrWorkspaceNotQuiescent
}

// KeepAttempt reserves bounded private blob custody before publishing the exact
// host journal ref. It is never seeded into a pod's readable attempt index.
func (b *parentInvocationBlobs) KeepAttempt(ctx context.Context, retained childpod.RetainedAttempt) error {
	data, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	if len(data) > childpod.MaxRetainedAttemptBytes {
		return errors.New("retained parent attempt exceeds bound")
	}
	if err = b.Put(ctx, journal.Digest(data), data); err != nil {
		return err
	}
	b.retainedRef, err = childpod.RecordRetainedAttempt(b.recorder, retained)
	if err == nil {
		b.retained = retained
	}
	return err
}
