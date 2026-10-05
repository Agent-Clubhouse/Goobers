package triggerqueue

import (
	"context"
	"database/sql"
	"errors"
)

// MaxChildProposalBytes bounds exact authored source, separately from the small
// start envelope. Proposals share the queue's database and byte headroom.
const MaxChildProposalBytes = 1 << 20

// ErrChildProposalUnavailable is fail-closed custody loss, including tampering.
// Resubmission must not silently repair previously accepted execution evidence.
var ErrChildProposalUnavailable = errors.New("triggerqueue: child proposal custody is missing or invalid")

// ChildProposal is exact source bytes with their sha256 content address. It is
// scoped by the accepting gaggle and retained while any full lineage refers to
// it. Acceptance commits both blob and reference, leaving no failed-intake blob.
type ChildProposal struct {
	Digest string
	Source []byte
}

const childProposalSchema = `
ALTER TABLE child_lineages ADD COLUMN proposal_digest TEXT NOT NULL DEFAULT '';
CREATE INDEX child_proposal_owners ON child_lineages(gaggle,proposal_digest,tombstoned_ns);
CREATE TABLE child_proposals (
 gaggle TEXT NOT NULL, digest TEXT NOT NULL,
 source BLOB NOT NULL CHECK(length(source) BETWEEN 1 AND 1048576),
 PRIMARY KEY(gaggle,digest)
);
-- The production lineage pruner owns proposal retention. Count references in
-- the transaction, so shared source survives until its last full owner expires.
CREATE TRIGGER child_release_proposal AFTER UPDATE OF tombstoned_ns ON child_lineages
WHEN OLD.tombstoned_ns IS NULL AND NEW.tombstoned_ns IS NOT NULL AND OLD.proposal_digest<>''
BEGIN
 DELETE FROM child_proposals WHERE gaggle=OLD.gaggle AND digest=OLD.proposal_digest
 AND NOT EXISTS(SELECT 1 FROM child_lineages c WHERE c.gaggle=OLD.gaggle AND c.proposal_digest=OLD.proposal_digest AND c.tombstoned_ns IS NULL);
END;
`

func (p *ChildProposal) validate() error {
	if p == nil {
		return nil
	}
	if len(p.Source) < 1 || len(p.Source) > MaxChildProposalBytes || p.Digest != "sha256:"+childDigest(p.Source) {
		return ErrChildProposalUnavailable
	}
	return nil
}

func (req ChildAcceptance) proposalBytes() int {
	if req.Proposal == nil {
		return 0
	}
	return len(req.Proposal.Source)
}

type childProposalReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readChildProposal(ctx context.Context, db childProposalReader, gaggle, digest string) (ChildProposal, error) {
	p := ChildProposal{Digest: digest}
	if err := db.QueryRowContext(ctx, `SELECT source FROM child_proposals WHERE gaggle=? AND digest=? AND length(source) BETWEEN 1 AND ?`, gaggle, digest, MaxChildProposalBytes).Scan(&p.Source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChildProposal{}, ErrChildProposalUnavailable
		}
		return ChildProposal{}, err
	}
	if err := p.validate(); err != nil {
		return ChildProposal{}, err
	}
	return p, nil
}

func keepChildProposal(ctx context.Context, tx *sql.Tx, gaggle string, proposal *ChildProposal) (string, error) {
	if proposal == nil {
		return "", nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_proposals WHERE gaggle=? AND digest=?`, gaggle, proposal.Digest).Scan(&exists); err != nil {
		return "", err
	}
	if exists != 0 {
		retained, err := readChildProposal(ctx, tx, gaggle, proposal.Digest)
		if err != nil {
			return "", err
		}
		if string(retained.Source) != string(proposal.Source) {
			return "", ErrChildProposalUnavailable
		}
		return proposal.Digest, nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_proposals(gaggle,digest,source) VALUES(?,?,?)`, gaggle, proposal.Digest, proposal.Source)
	return proposal.Digest, err
}

func verifyChildProposalRetry(ctx context.Context, tx *sql.Tx, child ChildRecord, requested *ChildProposal) error {
	digest := ""
	if requested != nil {
		digest = requested.Digest
	}
	if child.ProposalDigest != digest {
		return ErrConflict
	}
	if digest == "" || !child.TombstonedAt.IsZero() {
		return nil
	}
	retained, err := readChildProposal(ctx, tx, child.Identity.Gaggle, digest)
	if err != nil {
		return err
	}
	if string(retained.Source) != string(requested.Source) {
		return ErrChildProposalUnavailable
	}
	return nil
}

// ChildProposal returns digest-verified source owned by this full lineage.
// Like GetChild, it is internal: callers must check current parent/occurrence
// authority before the read. A digest alone is deliberately insufficient.
func (s *Store) ChildProposal(ctx context.Context, identity ChildIdentity) (ChildProposal, error) {
	child, err := s.GetChild(ctx, identity)
	if err != nil {
		return ChildProposal{}, err
	}
	if child.ProposalDigest == "" || !child.TombstonedAt.IsZero() {
		return ChildProposal{}, ErrChildProposalUnavailable
	}
	return readChildProposal(ctx, s.db, identity.Gaggle, child.ProposalDigest)
}
