package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// ParallelForkOptions binds one physical workspace to a branch in an exact
// durable parallel visit. The host must authorize and pin SnapshotSHA before
// calling these methods. A branch index alone is not a recovery identity.
type ParallelForkOptions struct {
	RepoURL          string
	OwnerRunID       string
	Gaggle           string
	ParallelSequence uint64
	Branch           int
	SnapshotSHA      string
}

// CreateParallelFromSnapshot creates an isolated retained workspace from an
// already-imported commit. Identical retries preserve its HEAD, index and files;
// conflicting or incomplete custody fails without deleting or resetting work.
// The source checkout and every sibling's checkout remain separate.
func (m *Manager) CreateParallelFromSnapshot(ctx context.Context, opts ParallelForkOptions) (*Worktree, error) {
	create, err := parallelForkCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	return m.createSnapshotWorkspace(ctx, create)
}

// AdoptParallelFromSnapshot verifies the existing exact fork on recovery. It
// performs no provisioning, network acquisition, checkout reset or recreation.
// A branch held for child custody instead requires the host's recorded
// StageCustody and AdoptHeldStage; a snapshot selector cannot release that hold.
func (m *Manager) AdoptParallelFromSnapshot(ctx context.Context, opts ParallelForkOptions) (*Worktree, error) {
	create, err := parallelForkCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	return m.adoptSnapshotWorkspace(ctx, create)
}

func parallelForkCreateOptions(opts ParallelForkOptions) (CreateOptions, error) {
	if !validRunID(opts.OwnerRunID) || len(opts.OwnerRunID) > 256 || opts.Gaggle == "" || len(opts.Gaggle) > 128 || opts.ParallelSequence == 0 || opts.Branch <= 0 || opts.Branch > 128 {
		return CreateOptions{}, fmt.Errorf("parallel fork requires bounded run, gaggle, visit and branch ownership")
	}
	// The immutable source is intentionally absent from this identity: changing
	// it on replay must collide with the original custody and be refused.
	identity := sha256.Sum256(fmt.Appendf(nil, "%q:%q:%d:%d", opts.Gaggle, opts.OwnerRunID, opts.ParallelSequence, opts.Branch))
	id := fmt.Sprintf("parallel-%x", identity[:16])
	create, err := childCreateOptions(ChildOptions{RepoURL: opts.RepoURL, RunID: id, OwnerRunID: opts.OwnerRunID, Gaggle: opts.Gaggle, SnapshotSHA: opts.SnapshotSHA})
	if err != nil {
		return CreateOptions{}, err
	}
	create.Branch = "goobers/parents/" + id
	return create, nil
}

// ParallelForkCustody computes the exact physical owner before creation, so the
// caller can durably reserve a whole fan-out before provisioning any checkout.
func ParallelForkCustody(opts ParallelForkOptions) (StageCustody, error) {
	create, err := parallelForkCreateOptions(opts)
	if err != nil {
		return StageCustody{}, err
	}
	return StageCustody{WorkspaceID: create.RunID, OwnerRunID: create.OwnerRunID, RepositoryDigest: RepositoryDigest(opts.RepoURL), Branch: create.Branch, StartRef: opts.SnapshotSHA}, nil
}
