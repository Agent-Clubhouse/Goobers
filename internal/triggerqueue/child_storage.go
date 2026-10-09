package triggerqueue

import (
	"context"
	"database/sql"
)

// Reserve the reference lifecycle's worst-case fork, result, disposition plan
// and request at admission. Each fork/result allows a 16 MiB artifact, 128 KiB
// receipt and 32 KiB database overhead. The workspace/result delivery slices
// consume these credits as retained bytes replace them; until then the complete
// reservation remains held. The database's 20% maintenance reserve is separate.
const (
	childSnapshotAllowance   = MaxChildSnapshotBytes + MaxChildSnapshotReceiptBytes + 32<<10
	childResultAllowance     = childSnapshotAllowance
	childPlanAllowance       = 128<<10 + 32<<10
	childRequestAllowance    = 4 << 10
	childCompletionAllowance = childSnapshotAllowance + childResultAllowance + childPlanAllowance + childRequestAllowance + childDispositionHistoryAllowance
)

const childStorageSchema = `
ALTER TABLE child_lineages ADD COLUMN reserved_bytes INTEGER NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0);
UPDATE child_lineages SET reserved_bytes=34050048
WHERE proposal_digest!='' AND tombstoned_ns IS NULL AND acknowledged_ns IS NULL;
`

func childStorageReservation(req ChildAcceptance) int {
	// Source-less internal fixtures cannot pass retained-source admission and
	// cannot acquire real workspace/result custody.
	if req.Proposal == nil {
		return 0
	}
	return childCompletionAllowance
}

func reserveChildStorage(ctx context.Context, tx *sql.Tx, identity ChildIdentity, amount int) error {
	_, err := tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=? WHERE gaggle=? AND parent_run=? AND occurrence=? AND invocation_key=?`, append([]any{amount}, childArgs(identity)...)...)
	return err
}

func consumeChildStorage(ctx context.Context, tx *sql.Tx, childID string, credit, additional int) error {
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT reserved_bytes FROM child_lineages WHERE child_id=?`, childID).Scan(&remaining); err != nil {
		return err
	}
	credit = min(credit, remaining)
	if err := childByteCapacity(ctx, tx, additional-credit); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE child_lineages SET reserved_bytes=reserved_bytes-? WHERE child_id=?`, credit, childID)
	return err
}

func resultStorageCredit(ctx context.Context, tx *sql.Tx, childID string) (int, error) {
	var snapshot string
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_digest FROM child_lineages WHERE child_id=?`, childID).Scan(&snapshot); err != nil {
		return 0, err
	}
	credit := childResultAllowance
	if snapshot == "" {
		credit += childSnapshotAllowance
	}
	return credit, nil
}
