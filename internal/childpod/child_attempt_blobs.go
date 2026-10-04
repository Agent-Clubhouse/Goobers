package childpod

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ChildAttemptBlobs is the signed physical contract's view of retained child
// custody. A digest alone cannot grant a sibling stage's data to this attempt.
type ChildAttemptBlobs struct {
	Store          ScopedBlobs
	ContractDigest string
}

// Describe returns a credential-free custody backend name.
func (s ChildAttemptBlobs) Describe() string { return "bounded-child-attempt-custody" }

// Get reads only exact contract-owned data.
func (s ChildAttemptBlobs) Get(ctx context.Context, digest string) ([]byte, error) {
	return s.GetBounded(ctx, digest, triggerqueue.MaxChildBlobBytes)
}

// GetBounded applies membership and byte bounds before allocation.
func (s ChildAttemptBlobs) GetBounded(ctx context.Context, digest string, limit int64) ([]byte, error) {
	if s.Store.Queue == nil {
		return nil, blobstore.ErrNotFound
	}
	data, err := s.Store.Queue.ChildAttemptBlobBounded(ctx, s.Store.Identity, s.ContractDigest, digest, limit)
	if errors.Is(err, triggerqueue.ErrChildBlobUnavailable) {
		return nil, blobstore.ErrNotFound
	}
	return data, err
}

// Put establishes ownership only after exact supplied bytes are validated.
func (s ChildAttemptBlobs) Put(ctx context.Context, digest string, data []byte) error {
	if s.Store.Queue == nil {
		return blobstore.ErrNotFound
	}
	return s.Store.Queue.KeepChildAttemptBlob(ctx, s.Store.Identity, s.ContractDigest, digest, data)
}

// Has checks presence only in this signed contract's custody.
func (s ChildAttemptBlobs) Has(ctx context.Context, digest string) (bool, error) {
	_, err := s.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// BindContract grants only this verified input, kit and declared context before
// dispatch. Ordinary authored/uploaded data cannot invoke this host authority.
func (s ScopedBlobs) BindContract(ctx context.Context, digest string) error {
	raw, err := s.Get(ctx, digest)
	if err != nil {
		return err
	}
	c, err := DecodeContract(raw, digest)
	if err != nil {
		return err
	}
	lineage := c.Identity.Child
	if c.ParentOrigin != nil || lineage == nil || c.Identity.Gaggle != s.Identity.Gaggle || lineage.ParentRunID != s.Identity.ParentRunID || lineage.StageOccurrence != s.Identity.StageOccurrence || lineage.InvocationKey != s.Identity.InvocationKey {
		return errors.New("child contract differs from retained lineage")
	}
	child, err := s.Queue.GetChild(ctx, s.Identity)
	if err != nil {
		return err
	}
	if c.Identity.RunID != child.RunID || lineage.SourceDigest != child.ProposalDigest {
		return errors.New("child contract source differs from accepted custody")
	}
	reads := append([]string{digest}, c.ContextDigests...)
	if c.KitDigest != "" {
		reads = append(reads, c.KitDigest)
	}
	for _, read := range reads {
		if _, err = s.Get(ctx, read); err != nil {
			return err
		}
		if err = s.Queue.AllowChildContractRead(ctx, s.Identity, digest, read); err != nil {
			return err
		}
	}
	return nil
}
