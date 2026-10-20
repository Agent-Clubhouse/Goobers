package claimsclient

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type administrativeLedger interface {
	RevokeSharedExecution(Key, sharedclaim.Owner, string) (bool, error)
	ForceReleaseEntry(Entry, string) error
}

// ForceReleaseExpected revokes shared execution before waiting for the item
// transition lock or resolving credentials. Cleanup retains custody on failure;
// a pending admission can never finalize over the persisted revocation.
func (f *File) ForceReleaseExpected(ctx context.Context, expected Entry, actor string) error {
	if actor == "" {
		return fmt.Errorf("claimsclient: administrative release requires an actor")
	}
	var cleanup bool
	err := f.Locked(ctx, "claims.shared.revoke", func(tx Ledger) error {
		session := tx.(*fileSession)
		current, held := matchingEntry(session.ledger, expected)
		if !held {
			return nil
		}
		ledger, ok := session.ledger.(administrativeLedger)
		if !ok {
			return fmt.Errorf("claimsclient: administrative release is unavailable")
		}
		if current.SharedDeadline.IsZero() {
			return ledger.ForceReleaseEntry(current, actor)
		}
		var err error
		cleanup, err = ledger.RevokeSharedExecution(KeyForEntry(current), current.SharedOwner, actor)
		return err
	})
	if err != nil || !cleanup {
		return err
	}
	_, err = f.releaseShared(ctx, expected, claimrelease.ForceReleased.Reason().By(actor), time.Time{})
	return err
}
