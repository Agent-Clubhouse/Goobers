package triggerqueue

import (
	"context"
	"database/sql"
)

// Worst-case custody is reserved when a real authored child is accepted. This
// prevents later starts from consuming space needed to finish an accepted child.
// Credits include SQLite row/page overhead; the existing 20% maintenance reserve
// remains independent. Reservations are released as actual bytes replace them.
const (
	childSnapshotAllowance   = MaxChildSnapshotBytes + MaxChildSnapshotReceiptBytes + 32*1024
	childResultAllowance     = childSnapshotAllowance
	childPlanAllowance       = MaxChildSnapshotReceiptBytes + 32*1024
	childRequestAllowance    = 4 * 1024
	childCompletionAllowance = childSnapshotAllowance + childResultAllowance + childPlanAllowance + childRequestAllowance + childDispositionHistoryAllowance
)

const childStorageSchema = `
ALTER TABLE child_lineages ADD COLUMN reserved_bytes INTEGER NOT NULL DEFAULT 0 CHECK(reserved_bytes>=0);
UPDATE child_lineages SET reserved_bytes=
 (CASE WHEN snapshot_digest='' AND result_digest='' THEN 16941056 ELSE 0 END)+
 (CASE WHEN result_digest='' THEN 16941056 ELSE 0 END)+
 (CASE WHEN disposition_digest='' THEN 4096 ELSE 0 END)+
 (CASE WHEN NOT EXISTS(SELECT 1 FROM child_dispositions d WHERE d.child_id=child_lineages.child_id AND d.plan_digest!='') THEN 163840 ELSE 0 END)
WHERE proposal_digest!='' AND tombstoned_ns IS NULL AND acknowledged_ns IS NULL;
`

func childStorageReservation(req ChildAcceptance) int {
	// Source-less internal lineage fixtures cannot pass the retained-source
	// execution validator and never acquire actual workspace/result custody.
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
