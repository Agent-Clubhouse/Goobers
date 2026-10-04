package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

// Routing limits count retained history as well as current work. Intake also
// reserves each possible start and group before acknowledging an event.
const (
	MaxEventDeliveries  = 50000
	MaxOpenEventGroups  = 1000
	MaxEventRootStarts  = 100
	eventRouteAllowance = MaxPayloadBytes + 16*1024
	eventGroupAllowance = MaxPayloadBytes + 4096
)

const eventGroupSchema = `
CREATE TABLE event_receipts_v2 (
 id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL, producer TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
 authority BLOB NOT NULL CHECK(length(authority) BETWEEN 1 AND 4096),
 digest TEXT NOT NULL, envelope BLOB NOT NULL CHECK(length(envelope)<=16384),
 plan BLOB NOT NULL CHECK(length(plan)<=16384), plan_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('routing_pending','accepted_unmatched','routing_failed','routed','routing_partial')),
 reason TEXT NOT NULL DEFAULT '' CHECK(length(reason)<=1024),
 accepted_ns INTEGER NOT NULL, finished_ns INTEGER, tombstoned_ns INTEGER,
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0),
 accepted_seq INTEGER NOT NULL UNIQUE,
 reserved_starts INTEGER NOT NULL CHECK(reserved_starts BETWEEN 0 AND 32),
 root_id TEXT NOT NULL,
 UNIQUE(gaggle,producer,source,event_id)
);
INSERT INTO event_receipts_v2 SELECT *,rowid,
 CASE WHEN state='routing_pending' AND json_valid(plan) THEN COALESCE(json_array_length(plan,'$.routes'),0) ELSE 0 END,
 CASE WHEN json_valid(authority) THEN COALESCE(NULLIF(json_extract(authority,'$.rootId'),''),id) ELSE id END
 FROM event_receipts;
DROP TABLE event_receipts;
ALTER TABLE event_receipts_v2 RENAME TO event_receipts;
UPDATE event_receipts SET reserved_bytes=reserved_starts*32768+4096 WHERE state='routing_pending';
CREATE INDEX event_pending ON event_receipts(gaggle,state,accepted_seq);
CREATE INDEX event_retention ON event_receipts(tombstoned_ns,finished_ns,id);
CREATE INDEX event_scope ON event_receipts(gaggle,tombstoned_ns,accepted_ns,id);
CREATE INDEX event_root_receipts ON event_receipts(gaggle,root_id);
CREATE TABLE event_sequence(id INTEGER PRIMARY KEY CHECK(id=1),value INTEGER NOT NULL);
INSERT INTO event_sequence VALUES(1,(SELECT COALESCE(MAX(accepted_seq),0) FROM event_receipts));
CREATE TABLE event_groups (
 id TEXT PRIMARY KEY NOT NULL,gaggle TEXT NOT NULL,consumer TEXT NOT NULL,revision TEXT NOT NULL,
 route BLOB NOT NULL CHECK(length(route) BETWEEN 1 AND 4096), route_digest TEXT NOT NULL,
 debounce_key TEXT NOT NULL, input_mode TEXT NOT NULL CHECK(input_mode IN ('all','latest')),
 state TEXT NOT NULL CHECK(state IN ('open','queued')),
 first_ns INTEGER NOT NULL,last_ns INTEGER NOT NULL,deadline_ns INTEGER NOT NULL,
 event_count INTEGER NOT NULL CHECK(event_count BETWEEN 1 AND 1000),
 selected_receipt TEXT NOT NULL,closed_ns INTEGER,close_reason TEXT NOT NULL DEFAULT '',
 acceptance_id TEXT NOT NULL DEFAULT '',settled_ns INTEGER,outcome TEXT NOT NULL DEFAULT '',
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0),reserved_starts INTEGER NOT NULL CHECK(reserved_starts BETWEEN 0 AND 1)
);
CREATE UNIQUE INDEX event_group_open ON event_groups(gaggle,consumer,revision,route_digest,debounce_key) WHERE state='open';
CREATE UNIQUE INDEX event_group_start ON event_groups(acceptance_id) WHERE acceptance_id<>'';
CREATE INDEX event_group_due ON event_groups(gaggle,state,deadline_ns,id);
CREATE TABLE event_deliveries (
 gaggle TEXT NOT NULL,receipt_id TEXT NOT NULL,consumer TEXT NOT NULL,group_id TEXT NOT NULL,receipt_seq INTEGER NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(receipt_id,consumer)
);
CREATE INDEX event_group_members ON event_deliveries(gaggle,group_id,receipt_seq,receipt_id);
CREATE INDEX event_receipt_members ON event_deliveries(receipt_id,group_id);
CREATE TRIGGER event_release_members AFTER DELETE ON event_receipts
BEGIN DELETE FROM event_deliveries WHERE receipt_id=OLD.id; END;
CREATE TABLE event_roots(gaggle TEXT NOT NULL,root_id TEXT NOT NULL,starts INTEGER NOT NULL CHECK(starts BETWEEN 0 AND 100),workflow INTEGER NOT NULL CHECK(workflow IN (0,1)),PRIMARY KEY(gaggle,root_id));
CREATE TABLE event_root_sources(gaggle TEXT NOT NULL,root_id TEXT NOT NULL,source_run_id TEXT NOT NULL,PRIMARY KEY(gaggle,root_id,source_run_id));
CREATE TABLE event_group_roots(gaggle TEXT NOT NULL,group_id TEXT NOT NULL,root_id TEXT NOT NULL,PRIMARY KEY(group_id,root_id));
CREATE INDEX event_group_root_scope ON event_group_roots(gaggle,root_id);
CREATE TRIGGER event_pin_start BEFORE DELETE ON triggers
WHEN EXISTS(SELECT 1 FROM event_groups WHERE acceptance_id=OLD.id)
BEGIN SELECT RAISE(IGNORE); END;
`

// EventGroup is a durable immutable closed input set or an open debounce group.
// Queued means a trigger exists, not that execution has begun or completed.
type EventGroup struct {
	ID, Gaggle, State, InputMode, SelectedReceipt, AcceptanceID, CloseReason, Outcome string
	Route                                                                             eventing.Route
	FirstAt, LastAt, Deadline, ClosedAt, SettledAt                                    time.Time
	Count                                                                             int
}

const eventGroupColumns = "id,gaggle,route,route_digest,input_mode,state,first_ns,last_ns,deadline_ns,event_count,selected_receipt,acceptance_id,closed_ns,close_reason,settled_ns,outcome"

func scanEventGroup(row scanner) (EventGroup, error) {
	var result EventGroup
	var raw []byte
	var digest string
	var first, last, deadline int64
	var closed, settled sql.NullInt64
	err := row.Scan(&result.ID, &result.Gaggle, &raw, &digest, &result.InputMode, &result.State, &first, &last, &deadline, &result.Count, &result.SelectedReceipt, &result.AcceptanceID, &closed, &result.CloseReason, &settled, &result.Outcome)
	if err != nil {
		return result, err
	}
	if "sha256:"+childDigest(raw) != digest {
		return result, ErrConflict
	}
	if err = json.Unmarshal(raw, &result.Route); err != nil {
		return result, err
	}
	if _, err = (eventing.Plan{Revision: result.Route.Revision, Routes: []eventing.Route{result.Route}}).Marshal(); err != nil {
		return result, err
	}
	result.FirstAt = time.Unix(0, first).UTC()
	result.LastAt = time.Unix(0, last).UTC()
	result.Deadline = time.Unix(0, deadline).UTC()
	if closed.Valid {
		result.ClosedAt = time.Unix(0, closed.Int64).UTC()
	}
	if settled.Valid {
		result.SettledAt = time.Unix(0, settled.Int64).UTC()
	}
	return result, nil
}

// EventGroup returns only within the already authorized gaggle partition.
func (s *Store) EventGroup(ctx context.Context, gaggle, id string) (EventGroup, error) {
	return scanEventGroup(s.db.QueryRowContext(ctx, "SELECT "+eventGroupColumns+" FROM event_groups WHERE gaggle=? AND id=?", gaggle, id))
}

func eventRoutingCapacity(ctx context.Context, tx *sql.Tx, gaggle string, routes int) error {
	if err := triggerSlotCapacity(ctx, tx, routes); err != nil {
		return err
	}
	var groups, members, pending int
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM event_groups WHERE gaggle=? AND state='open'),
 (SELECT COUNT(*) FROM event_deliveries WHERE gaggle=?),
 (SELECT COALESCE(SUM(reserved_starts),0) FROM event_receipts WHERE gaggle=? AND state='routing_pending')`, gaggle, gaggle, gaggle).Scan(&groups, &members, &pending); err != nil {
		return err
	}
	if groups+pending+routes > MaxOpenEventGroups || members+pending+routes > MaxEventDeliveries {
		return ErrFull
	}
	return nil
}

func triggerSlotCapacity(ctx context.Context, tx *sql.Tx, additional int) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM triggers)+
 (SELECT COALESCE(SUM(reserved_starts),0) FROM event_receipts)+
 (SELECT COALESCE(SUM(reserved_starts),0) FROM event_groups)`).Scan(&count); err != nil {
		return err
	}
	if count+additional > MaxRecords {
		return ErrFull
	}
	return nil
}

func validEventBatch(gaggle string, now time.Time, limit int) error {
	if !validChildText(gaggle, 128, true) || now.IsZero() || limit < 1 || limit > 100 {
		return errors.New("triggerqueue: invalid event routing scope, time or batch")
	}
	return nil
}

// ErrEventChainLimit is a permanent consumer refusal, unlike retryable storage
// capacity. Other consumers of the same receipt can still receive delivery.
var ErrEventChainLimit = errors.New("triggerqueue: event root start budget exhausted")

func eventRoot(receipt EventReceipt) string {
	if receipt.Producer.RootID != "" {
		return receipt.Producer.RootID
	}
	return receipt.ID
}

func reserveEventRoot(ctx context.Context, tx *sql.Tx, receipt EventReceipt, groupID string) error {
	root := eventRoot(receipt)
	if err := retainEventRootSource(ctx, tx, receipt, root); err != nil {
		return err
	}
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_group_roots WHERE gaggle=? AND group_id=? AND root_id=?`, receipt.Producer.Gaggle, groupID, root).Scan(&found); err != nil {
		return err
	}
	if found > 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE event_roots SET starts=starts+1 WHERE gaggle=? AND root_id=? AND starts<?`, receipt.Producer.Gaggle, root, MaxEventRootStarts)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrEventChainLimit
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO event_group_roots(gaggle,group_id,root_id) VALUES(?,?,?)`, receipt.Producer.Gaggle, groupID, root)
	return err
}

func retainEventRootSource(ctx context.Context, tx *sql.Tx, receipt EventReceipt, root string) error {
	workflow := receipt.Producer.RunID != ""
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_roots(gaggle,root_id,starts,workflow) VALUES(?,?,0,?) ON CONFLICT(gaggle,root_id) DO UPDATE SET workflow=MAX(workflow,excluded.workflow)`, receipt.Producer.Gaggle, root, workflow); err != nil {
		return err
	}
	if !workflow {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO event_root_sources(gaggle,root_id,source_run_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, receipt.Producer.Gaggle, root, receipt.Producer.RunID)
	return err
}
