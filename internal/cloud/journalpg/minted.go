package journalpg

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

var errMintNotCommitted = errors.New("journalpg: ownership mint did not commit; retry")

type mintedOwner struct {
	create      bool
	token       journal.OwnerToken
	epoch       int64
	predecessor journal.OwnerToken
}

// beginMint reserves publication, not database authority. Keep the reservation
// through keepRun so a retry cannot publish the same unresolved capability twice.
func (b *Backend) beginMint(key journal.RunKey, epoch int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	owned := b.minting[key]
	for handle, r := range b.runs {
		if r.epoch < epoch {
			delete(b.runs, handle)
		} else if r.key == key {
			owned = true
		}
	}
	for key, r := range b.unresolved {
		if r.epoch < epoch {
			delete(b.unresolved, key)
		}
	}
	if !owned {
		b.minting[key] = true
	}
	return !owned
}

func (b *Backend) endMint(key journal.RunKey) {
	b.mu.Lock()
	delete(b.minting, key)
	b.mu.Unlock()
}

// A second unknown rotation can leave either incarnation durable. Resolve the
// pair before minting again, so uncertainty never grows beyond two tokens.
func (b *Backend) unresolvedOwner(ctx context.Context, key journal.RunKey, epoch int64, creating bool) (*string, error) {
	b.mu.Lock()
	r, ok := b.unresolved[key]
	b.mu.Unlock()
	if !ok || r.epoch != epoch {
		return nil, nil
	}
	// A recovery can have history and pending transcript cleanup. Creation
	// must neither publish that writer as fresh nor consume its retry proof.
	if creating && !r.create {
		return nil, os.ErrExist
	}
	if r.predecessor != "" {
		token, storedEpoch, err := b.readMinted(ctx, key)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: resolve predecessor: %w", lease.ErrCommitUnknown, err)
		}
		if token == nil || storedEpoch != epoch || (*token != string(r.token) && *token != string(r.predecessor)) {
			b.forgetUnresolved(key)
			return nil, nil
		}
		r.token, r.predecessor = journal.OwnerToken(*token), ""
		b.mu.Lock()
		b.unresolved[key] = r
		b.mu.Unlock()
	}
	token := string(r.token)
	return &token, nil
}

func (b *Backend) forgetUnresolved(key journal.RunKey) {
	b.mu.Lock()
	delete(b.unresolved, key)
	b.mu.Unlock()
}

func (b *Backend) resolveMintError(ctx context.Context, r *runOwner, predecessor *string, create bool, err error) error {
	if !errors.Is(err, lease.ErrCommitUnknown) {
		return err
	}
	return b.resolveMinted(ctx, r, predecessor, create)
}

// resolveMinted reads through the pool, never through the failed transaction.
// Only the exact capability minted in this epoch proves that COMMIT succeeded.
// An unavailable read retains that capability for a later fenced recovery.
func (b *Backend) resolveMinted(ctx context.Context, r *runOwner, predecessor *string, create bool) error {
	token, epoch, err := b.readMinted(ctx, r.key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		pending := mintedOwner{token: r.token, epoch: r.epoch, create: create}
		if predecessor != nil {
			pending.predecessor = journal.OwnerToken(*predecessor)
		}
		b.mu.Lock()
		b.unresolved[r.key] = pending
		b.mu.Unlock()
		return fmt.Errorf("%w: resolve journal owner: %w", lease.ErrCommitUnknown, err)
	}
	b.forgetUnresolved(r.key)
	if err != nil || token == nil || *token != string(r.token) || epoch != r.epoch {
		// A failed rotation can leave our previously unresolved capability intact.
		if token != nil && predecessor != nil && *token == *predecessor && epoch == r.epoch {
			b.mu.Lock()
			b.unresolved[r.key] = mintedOwner{token: journal.OwnerToken(*token), epoch: epoch}
			b.mu.Unlock()
		}
		return errMintNotCommitted
	}
	return nil
}

func (b *Backend) readMinted(ctx context.Context, key journal.RunKey) (*string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, lease.FencedTransactionTimeout)
	defer cancel()
	var token *string
	var epoch int64
	err := b.config.Pool.QueryRow(ctx, `SELECT owner_token,owner_epoch FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2`, b.config.InstanceID, key.RunID).Scan(&token, &epoch)
	return token, epoch, err
}
