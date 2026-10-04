package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// beginChildRestartRuntime holds revocable human authority for the complete
// contained epoch, including worker teardown/import after cancellation. Broker
// calls still repeat exact current stage/queue authority before minting tokens.
func (s *daemonCredentialService) beginChildRestartRuntime(ctx context.Context, id journal.RunIdentity, _ runner.SecretRegistrar) (context.Context, func(), error) {
	defs, releasePin, err := pinnedCredentialDefinitions(ctx, s.layout, id.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	owned, cleanup, err := s.beginChildRestartAuthority(ctx, id, defs)
	if err != nil {
		releasePin()
		return nil, nil, err
	}
	return owned, func() {
		// Retain the immutable generation when the worker remains unjoined.
		// The queue is its additional durable owner across daemon restarts.
		if cleanup() == nil {
			releasePin()
		}
	}, nil
}

func (s *daemonCredentialService) beginChildRestartAuthority(ctx context.Context, id journal.RunIdentity, defs credentialPlaneDefinitions) (context.Context, func() error, error) {
	if s == nil || s.interactive == nil || id.Child == nil || id.Child.ExecutionEpoch == 0 {
		return nil, nil, interactiveaccess.ErrDenied
	}
	if _, err := retainedChildExecutionRef(ctx, s.childQueue, id, true); err != nil {
		return nil, nil, err
	}
	dir, err := s.layout.FindRunDir(id.RunID)
	if err != nil {
		return nil, nil, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, nil, err
	}
	authority, err := interactiveaccess.LoadRestartAuthority(reader, id)
	if err != nil {
		return nil, nil, err
	}
	scope, ok := defs.Scopes[id.Gaggle]
	if !ok {
		return nil, nil, interactiveaccess.ErrDenied
	}
	// Reload lock order is interactive policy -> child authority -> scheduler.
	// No child authority lock is held here; subsequent broker calls reuse this
	// immutable human lease instead of acquiring policy under a child lock.
	lease, err := s.interactive.BeginExecution(ctx, authority.Principal(), id.Gaggle)
	if err != nil {
		return nil, nil, err
	}
	if err := lease.RequireSources(scope.Project, scope.Backlog, scope.AdditionalRepos); err != nil {
		lease.Close()
		return nil, nil, err
	}
	owned, proof := invoke.WithWorkspaceQuiescence(lease.Context())
	owned = context.WithValue(owned, childInteractiveCredentialKey{}, &childInteractiveCredentialScope{run: id.RunID, lease: lease, sources: scope})
	return owned, func() error {
		err := proof.VerifyIdle()
		return errors.Join(err, lease.CloseAfter(err))
	}, nil
}
