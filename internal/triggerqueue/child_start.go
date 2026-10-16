package triggerqueue

import "context"

// VerifiedChildStart verifies immutable accepted bytes against lineage custody. It is
// an internal read; the caller must first authorize this parent occurrence.
func (s *Store) VerifiedChildStart(ctx context.Context, identity ChildIdentity, actor string) (Record, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return Record{}, err
	}
	if !child.TombstonedAt.IsZero() {
		return Record{}, ErrTransition
	}
	receipt, err := s.Get(ctx, child.AcceptanceID, actor)
	if err != nil {
		return Record{}, err
	}
	var actorDigest, payloadDigest string
	err = s.db.QueryRowContext(ctx, `SELECT actor_digest,payload_digest FROM child_lineages c`+childWhere, childArgs(identity)...).Scan(&actorDigest, &payloadDigest)
	if err != nil {
		return Record{}, err
	}
	if actorDigest != childDigest([]byte(actor)) || payloadDigest != childDigest(receipt.Payload) || receipt.Key != child.StartKey {
		return Record{}, ErrConflict
	}
	return receipt, nil
}
