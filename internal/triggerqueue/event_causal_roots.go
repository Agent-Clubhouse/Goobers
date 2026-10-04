package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// MaxEventCausalRoots bounds one publication's immutable inherited root set.
// Larger consumer groups remain valid inputs but cannot publish another event.
const MaxEventCausalRoots = 32

// ErrEventRootSet refuses missing, altered or over-bound causal authority.
var ErrEventRootSet = errors.New("triggerqueue: event causal roots unavailable, mismatched, or over limit")

const eventRootSetSchema = `
CREATE TABLE event_receipt_roots (
 gaggle TEXT NOT NULL,receipt_id TEXT NOT NULL,root_id TEXT NOT NULL,
 PRIMARY KEY(receipt_id,root_id)
);
CREATE INDEX event_receipt_root_scope ON event_receipt_roots(gaggle,root_id);
CREATE TRIGGER event_release_roots AFTER DELETE ON event_receipts
BEGIN DELETE FROM event_receipt_roots WHERE receipt_id=OLD.id; END;
`

func acceptedEventRoots(ctx context.Context, tx *sql.Tx, p EventProducer) ([]string, error) {
	var roots []string
	if p.RootGroupID != "" {
		var err error
		roots, err = consumerRootSet(ctx, tx, p.Gaggle, p.RootGroupID, p.RunID, p.Depth)
		if err != nil {
			return nil, err
		}
		if p.RootSetDigest != eventRootsDigest(roots) {
			return nil, ErrEventRootSet
		}
	} else if p.RunID != "" {
		var consumers int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_groups WHERE gaggle=? AND acceptance_id=?`, p.Gaggle, "trigger-"+p.RunID).Scan(&consumers); err != nil {
			return nil, err
		}
		if consumers != 0 {
			return nil, ErrEventRootSet
		}
		roots = []string{p.RootID}
	}
	if p.RunID != "" {
		known, err := readRootIDs(ctx, tx, `SELECT root_id FROM event_root_sources WHERE gaggle=? AND source_run_id=? ORDER BY root_id LIMIT ?`, p.Gaggle, p.RunID, MaxEventCausalRoots+1)
		if err != nil {
			return nil, err
		}
		if len(known) > 0 && !slices.Equal(known, roots) {
			return nil, ErrEventRootSet
		}
	}
	return roots, nil
}

func consumerRootSet(ctx context.Context, tx *sql.Tx, gaggle, groupID, runID string, depth int) ([]string, error) {
	var acceptance, state string
	err := tx.QueryRowContext(ctx, `SELECT acceptance_id,state FROM event_groups WHERE gaggle=? AND id=?`, gaggle, groupID).Scan(&acceptance, &state)
	if err != nil {
		return nil, errors.Join(ErrEventRootSet, err)
	}
	if state != "queued" || acceptance != "trigger-"+runID {
		return nil, ErrEventRootSet
	}
	expected, err := consumerDepth(ctx, tx, gaggle, groupID)
	if err != nil {
		return nil, err
	}
	if depth != expected || depth > 8 {
		return nil, ErrEventRootSet
	}
	roots, err := readRootIDs(ctx, tx, `SELECT root_id FROM event_group_roots WHERE gaggle=? AND group_id=? ORDER BY root_id LIMIT ?`, gaggle, groupID, MaxEventCausalRoots+1)
	if err != nil {
		return nil, err
	}
	if len(roots) < 1 || len(roots) > MaxEventCausalRoots {
		return nil, ErrEventRootSet
	}
	return roots, nil
}

func consumerDepth(ctx context.Context, tx *sql.Tx, gaggle, groupID string) (int, error) {
	var depth sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT MAX(json_extract(r.authority,'$.depth')) FROM event_deliveries d JOIN event_receipts r ON r.id=d.receipt_id WHERE d.gaggle=? AND d.group_id=? AND r.gaggle=?`, gaggle, groupID, gaggle).Scan(&depth)
	if err != nil {
		return 0, err
	}
	if !depth.Valid {
		return 0, ErrEventRootSet
	}
	return int(depth.Int64) + 1, nil
}

func readRootIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var roots []string
	for rows.Next() {
		var root string
		if err = rows.Scan(&root); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

func retainReceiptRoots(ctx context.Context, tx *sql.Tx, receipt EventReceipt, roots []string) error {
	for _, root := range roots {
		if receipt.Producer.RootGroupID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO event_receipt_roots(gaggle,receipt_id,root_id) VALUES(?,?,?)`, receipt.Producer.Gaggle, receipt.ID, root); err != nil {
				return err
			}
		}
		if receipt.Producer.RunID != "" {
			if err := retainEventRootSource(ctx, tx, receipt, root); err != nil {
				return err
			}
		}
	}
	return nil
}

func receiptRootSet(ctx context.Context, tx *sql.Tx, receipt EventReceipt) ([]string, error) {
	// Historical and ordinary receipts already commit one root in producer
	// authority. Do not duplicate an entire retained database during upgrade.
	if receipt.Producer.RootGroupID == "" {
		return []string{eventRoot(receipt)}, nil
	}
	roots, err := readRootIDs(ctx, tx, `SELECT root_id FROM event_receipt_roots WHERE gaggle=? AND receipt_id=? ORDER BY root_id LIMIT ?`, receipt.Producer.Gaggle, receipt.ID, MaxEventCausalRoots+1)
	if err != nil {
		return nil, err
	}
	if len(roots) < 1 || len(roots) > MaxEventCausalRoots {
		return nil, ErrEventRootSet
	}
	if receipt.Producer.RootSetDigest != eventRootsDigest(roots) {
		return nil, ErrEventRootSet
	}
	return roots, nil
}

func eventRootsDigest(roots []string) string {
	raw, _ := json.Marshal(roots)
	return "sha256:" + childDigest(raw)
}

func eventRootReservation(consumers, roots int) int {
	// Includes receipt-owned root rows and indexed per-consumer root links.
	return eventRoutingReservation(consumers) + roots*4096 + consumers*max(0, roots-1)*4096
}

// EventConsumerProducer fills only causal authority for a journal-verified
// consumer. The host must first verify its retained journal and active stage;
// calling this store API is not authentication. No consumer body selects roots.
func (s *Store) EventConsumerProducer(ctx context.Context, p EventProducer, groupID string) (EventProducer, error) {
	if p.RootID != "" || p.RootGroupID != "" || p.RootSetDigest != "" || p.CausationID != "" || p.Depth != 0 || p.RunID == "" || strings.TrimSpace(groupID) == "" {
		return EventProducer{}, ErrEventRootSet
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventProducer{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p.Depth, err = consumerDepth(ctx, tx, p.Gaggle, groupID)
	if err != nil {
		return EventProducer{}, err
	}
	p.RootGroupID, p.CausationID = groupID, groupID
	roots, err := consumerRootSet(ctx, tx, p.Gaggle, groupID, p.RunID, p.Depth)
	if err != nil {
		return EventProducer{}, err
	}
	p.RootSetDigest = eventRootsDigest(roots)
	if !p.Valid() {
		return EventProducer{}, ErrEventRootSet
	}
	return p, nil
}
