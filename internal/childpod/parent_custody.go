package childpod

import (
	"context"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ParentWriterStarted records host-owned physical writer custody.
const ParentWriterStarted = "isolated.parent.writer.started"

// ParentWriterJoined records the host's acknowledgement that a writer stopped.
const ParentWriterJoined = "isolated.parent.writer.joined"

// ParentPodScope binds a physical writer receipt to its immutable contract.
type ParentPodScope struct {
	Event    journal.Event
	Contract Contract
}

// ParentCustodyPending authorizes bounded late surrender for exactly one
// previously dispatched physical contract. It grants no execution, credentials,
// new child authority, or cross-attempt blob access.
func ParentCustodyPending(ctx context.Context, reader *journal.Reader, contractDigest string) (bool, error) {
	if !blobstore.ValidDigest(contractDigest) {
		return false, invoke.ErrWorkspaceNotQuiescent
	}
	pending, events, err := PendingParentScopes(ctx, reader)
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

// PendingParentScopes projects unjoined physical writers from trusted receipts.
func PendingParentScopes(ctx context.Context, reader *journal.Reader) (map[string]ParentPodScope, []journal.Event, error) {
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
	store := ParentBlobs{RunDir: reader.Dir(), Identity: id}
	pending := map[string]ParentPodScope{}
	seen := map[string]bool{}
	physical := map[int]bool{}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		kind, _ := event.Runner["kind"].(string)
		if event.Type != journal.EventRunnerAnnotation || (kind != ParentWriterStarted && kind != ParentWriterJoined) {
			continue
		}
		digest, ok := event.Runner["contractDigest"].(string)
		if !ok || !blobstore.ValidDigest(digest) {
			return nil, nil, invoke.ErrWorkspaceNotQuiescent
		}
		if kind == ParentWriterStarted {
			contract, err := readParentPodContract(ctx, store, events, event, digest)
			if err != nil || seen[digest] || physical[contract.PodAttempt] {
				return nil, nil, errors.Join(invoke.ErrWorkspaceNotQuiescent, err)
			}
			seen[digest], physical[contract.PodAttempt] = true, true
			pending[digest] = ParentPodScope{Event: event, Contract: contract}
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

func readParentPodContract(ctx context.Context, store ParentBlobs, events []journal.Event, event journal.Event, digest string) (Contract, error) {
	var contract Contract
	data, err := store.Get(ctx, digest)
	if err != nil {
		return contract, err
	}
	contract, err = DecodeContract(data, digest)
	if err != nil || contract.ParentOrigin == nil || !reflect.DeepEqual(contract.Identity, store.Identity) || contract.Stage != event.Stage || contract.Attempt != event.Attempt || contract.ParentBranch != event.Branch {
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
