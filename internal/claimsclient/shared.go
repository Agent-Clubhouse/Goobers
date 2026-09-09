package claimsclient

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/sharedclaim"
)

// SharedClaimBinding is resolved from trusted instance/run configuration,
// never from a stage-requested mode or incarnation token.
type SharedClaimBinding struct {
	Store     sharedclaim.Store
	RemoteKey string
	Owner     sharedclaim.Owner
}

// SharedClaimResolver supplies the provider binding at the file-client seam.
// Admission returns nil only for an explicitly resolved local policy. Release
// must resolve the persisted binding even when its owning run is terminal;
// current workflow settings must not turn a shared release into local cleanup.
// Both methods run inside the caller's claims lock and must not re-enter it.
type SharedClaimResolver interface {
	Admission(context.Context, Key, string, string) (*SharedClaimBinding, error)
	Release(context.Context, Entry) (SharedClaimBinding, error)
}

type sharedFileLedger interface {
	ClaimSharedScoped(context.Context, sharedclaim.Store, string, Key, sharedclaim.Owner, string, time.Duration) (bool, string, error)
	ReleaseCoordinatedShared(context.Context, sharedclaim.Store, string, Key, sharedclaim.Owner) error
}

func (s *fileSession) claimShared(ctx context.Context, key Key, runID, workflow string, ttl time.Duration, binding SharedClaimBinding) (bool, string, error) {
	if binding.Owner.Run != runID {
		return false, "", fmt.Errorf("claimsclient: shared admission belongs to another run")
	}
	ledger, ok := s.ledger.(sharedFileLedger)
	if !ok {
		return false, "", fmt.Errorf("claimsclient: shared admission is unavailable")
	}
	return ledger.ClaimSharedScoped(ctx, binding.Store, binding.RemoteKey, key, binding.Owner, workflow, ttl)
}

func (s *fileSession) releaseShared(ctx context.Context, entry Entry) error {
	if s.file.cfg.Shared == nil {
		return fmt.Errorf("claimsclient: shared release requires its provider resolver")
	}
	binding, err := s.file.cfg.Shared.Release(ctx, entry)
	if err != nil {
		return err
	}
	if binding.Owner != entry.SharedOwner {
		return fmt.Errorf("claimsclient: shared release belongs to another incarnation")
	}
	ledger, ok := s.ledger.(sharedFileLedger)
	if !ok {
		return fmt.Errorf("claimsclient: shared release is unavailable")
	}
	return ledger.ReleaseCoordinatedShared(ctx, binding.Store, binding.RemoteKey, KeyForEntry(entry), entry.SharedOwner)
}

func (s *fileSession) releaseEntry(ctx context.Context, entry Entry, runID string) error {
	if !entry.SharedDeadline.IsZero() {
		return s.releaseShared(ctx, entry)
	}
	return s.ledger.ReleaseEntry(entry, runID)
}
