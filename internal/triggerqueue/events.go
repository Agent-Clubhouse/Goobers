package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

// Event limits bound intake, routing custody and advertised replay retention.
const (
	MaxEventReceipts        = 10000
	MaxEventTombstones      = 100000
	EventRetention          = 7 * 24 * time.Hour
	EventTombstoneRetention = 30 * 24 * time.Hour
	EventRoutingDeadline    = time.Hour
)

// EventState describes receipt custody, never consumer execution completion.
type EventState string

// Receipt states deliberately distinguish no-match success from routing failure.
const (
	EventRoutingPending EventState = "routing_pending"
	EventUnmatched      EventState = "accepted_unmatched"
	EventRoutingFailed  EventState = "routing_failed"
	// EventRouted means every matched consumer owns a durable delivery.
	EventRouted EventState = "routed"
	// EventRoutingPartial preserves successful and failed consumer deliveries.
	EventRoutingPartial EventState = "routing_partial"
)

// EventProducer is derived from authenticated ingress/run ownership. Data and
// CloudEvents extensions cannot supply these values. Actor survives credential
// rotation; changing the actor cannot take over an existing producer identity.
type EventProducer = eventing.Producer

// EventAcceptance pins one validated envelope and matched routing revision.
// The runtime service, not an ingress body, constructs Producer and Plan.
type EventAcceptance struct {
	Producer EventProducer
	Envelope []byte
	Plan     eventing.Plan
}

// EventReceipt retains exact normalized source and route decisions. Tombstones
// retain identity/digest but omit the envelope and plan after the replay window.
type EventReceipt struct {
	ID           string
	Producer     EventProducer
	Source       string
	EventID      string
	Digest       string
	Envelope     []byte
	Plan         []byte
	PlanDigest   string
	State        EventState
	Reason       string
	AcceptedAt   time.Time
	FinishedAt   time.Time
	TombstonedAt time.Time
	Sequence     int64
}

const eventSchema = `
CREATE TABLE event_receipts (
 id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL, producer TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
 authority BLOB NOT NULL CHECK(length(authority) BETWEEN 1 AND 4096),
 digest TEXT NOT NULL, envelope BLOB NOT NULL CHECK(length(envelope)<=16384),
 plan BLOB NOT NULL CHECK(length(plan)<=16384), plan_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('routing_pending','accepted_unmatched','routing_failed')),
 reason TEXT NOT NULL DEFAULT '' CHECK(length(reason)<=1024),
 accepted_ns INTEGER NOT NULL, finished_ns INTEGER, tombstoned_ns INTEGER,
 reserved_bytes INTEGER NOT NULL CHECK(reserved_bytes>=0),
 UNIQUE(gaggle,producer,source,event_id)
);
CREATE INDEX event_pending ON event_receipts(state,accepted_ns,id);
CREATE INDEX event_retention ON event_receipts(tombstoned_ns,finished_ns,id);
CREATE INDEX event_scope ON event_receipts(gaggle,tombstoned_ns,accepted_ns,id);
`

const eventColumns = "id,authority,source,event_id,digest,envelope,plan,plan_digest,state,reason,accepted_ns,finished_ns,tombstoned_ns,accepted_seq"

func scanEvent(row scanner) (EventReceipt, error) {
	var result EventReceipt
	var authority []byte
	var accepted int64
	var finished, tombstoned sql.NullInt64
	err := row.Scan(&result.ID, &authority, &result.Source, &result.EventID, &result.Digest, &result.Envelope, &result.Plan, &result.PlanDigest, &result.State, &result.Reason, &accepted, &finished, &tombstoned, &result.Sequence)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(authority, &result.Producer); err != nil {
		return EventReceipt{}, err
	}
	result.AcceptedAt = time.Unix(0, accepted).UTC()
	if finished.Valid {
		result.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	if tombstoned.Valid {
		result.TombstonedAt = time.Unix(0, tombstoned.Int64).UTC()
	}
	return result, verifyEventReceipt(result)
}

func verifyEventReceipt(receipt EventReceipt) error {
	if receipt.Sequence < 1 || !validEventProducer(receipt.Producer) {
		return errors.New("triggerqueue: invalid retained event authority")
	}
	if !receipt.TombstonedAt.IsZero() {
		if len(receipt.Envelope) != 0 || len(receipt.Plan) != 0 || receipt.FinishedAt.IsZero() {
			return errors.New("triggerqueue: invalid event tombstone")
		}
		return nil
	}
	envelope, err := eventing.Parse(receipt.Envelope)
	if err != nil || envelope.Digest != receipt.Digest || envelope.Source != receipt.Source || envelope.ID != receipt.EventID || "sha256:"+childDigest(receipt.Plan) != receipt.PlanDigest {
		return errors.New("triggerqueue: retained event custody is missing or invalid")
	}
	return nil
}

// AcceptEvent commits receipt, authority and routing custody in one transaction.
// A duplicate recovers the original plan even after subscription changes. A
// changed envelope or authority conflicts; callers retry ambiguous outcomes with
// the original source/id. No-match publication is a durable successful receipt.
func (s *Store) AcceptEvent(ctx context.Context, req EventAcceptance, now time.Time) (EventReceipt, bool, error) {
	envelope, authority, plan, err := validateEventAcceptance(req, now)
	if err != nil {
		return EventReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventReceipt{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE gaggle=? AND producer=? AND source=? AND event_id=?", req.Producer.Gaggle, req.Producer.Binding, envelope.Source, envelope.ID))
	if err == nil {
		if existing.Digest != envelope.Digest || existing.Producer != req.Producer {
			return EventReceipt{}, false, ErrConflict
		}
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EventReceipt{}, false, err
	}
	roots, err := acceptedEventRoots(ctx, tx, req.Producer)
	if err != nil {
		return EventReceipt{}, false, err
	}
	reserved := eventRootReservation(len(req.Plan.Routes), max(1, len(roots)))
	if err := eventRoutingCapacity(ctx, tx, req.Producer.Gaggle, len(req.Plan.Routes)); err != nil {
		return EventReceipt{}, false, err
	}
	if err := eventIntakeCapacity(ctx, tx, req.Producer.Gaggle, len(envelope.JSON)+len(plan)+len(authority)+reserved); err != nil {
		return EventReceipt{}, false, err
	}
	result := EventReceipt{ID: fmt.Sprintf("event-%x", randomID()), Producer: req.Producer, Source: envelope.Source, EventID: envelope.ID, Digest: envelope.Digest, Envelope: envelope.JSON, Plan: plan, State: EventRoutingPending, AcceptedAt: now.UTC()}
	result.PlanDigest = "sha256:" + childDigest(plan)
	var finished any
	if len(req.Plan.Routes) == 0 {
		result.State, result.FinishedAt = EventUnmatched, now.UTC()
		finished, reserved = now.UnixNano(), 0
	}
	if err = tx.QueryRowContext(ctx, `UPDATE event_sequence SET value=value+1 WHERE id=1 RETURNING value`).Scan(&result.Sequence); err != nil {
		return EventReceipt{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO event_receipts(id,gaggle,producer,source,event_id,authority,digest,envelope,plan,plan_digest,state,accepted_ns,finished_ns,reserved_bytes,accepted_seq,reserved_starts,root_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, result.ID, result.Producer.Gaggle, result.Producer.Binding, result.Source, result.EventID, authority, result.Digest, result.Envelope, result.Plan, result.PlanDigest, result.State, now.UnixNano(), finished, reserved, result.Sequence, len(req.Plan.Routes), eventRoot(result))
	if err != nil {
		return EventReceipt{}, false, err
	}
	if len(roots) == 0 {
		roots = []string{result.ID}
	}
	if err = retainReceiptRoots(ctx, tx, result, roots); err != nil {
		return EventReceipt{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return EventReceipt{}, false, err
	}
	return result, false, nil
}

func validateEventAcceptance(req EventAcceptance, now time.Time) (eventing.Envelope, []byte, []byte, error) {
	if now.IsZero() || !validEventProducer(req.Producer) {
		return eventing.Envelope{}, nil, nil, errors.New("triggerqueue: invalid event authority or time")
	}
	envelope, err := eventing.Parse(req.Envelope)
	if err != nil {
		return envelope, nil, nil, err
	}
	plan, err := req.Plan.Marshal()
	if err != nil {
		return envelope, nil, nil, err
	}
	authority, err := json.Marshal(req.Producer)
	if err != nil || len(authority) > 4096 {
		return envelope, nil, nil, errors.New("triggerqueue: event authority exceeds limit")
	}
	return envelope, authority, plan, nil
}

func validEventProducer(p EventProducer) bool { return p.Valid() }

func eventRoutingReservation(consumers int) int {
	if consumers == 0 {
		return 0
	}
	// Reserve the maximum ordinary start payload and row/index overhead per
	// consumer plus bounded receipt finalization, before accepting custody.
	return consumers*eventRouteAllowance + 4096
}

func eventIntakeCapacity(ctx context.Context, tx *sql.Tx, gaggle string, additional int) error {
	var live, tombs int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(tombstoned_ns IS NULL),0),COALESCE(SUM(tombstoned_ns IS NOT NULL),0) FROM event_receipts WHERE gaggle=?`, gaggle).Scan(&live, &tombs)
	if err != nil {
		return err
	}
	if live >= MaxEventReceipts || tombs >= MaxEventTombstones {
		return ErrFull
	}
	return childByteCapacity(ctx, tx, additional)
}

// Event returns a scoped receipt for a service that has already authorized the
// caller against that gaggle and binding. There is no unscoped public lookup.
func (s *Store) Event(ctx context.Context, gaggle, binding, id string) (EventReceipt, error) {
	return scanEvent(s.db.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE gaggle=? AND producer=? AND id=?", gaggle, binding, id))
}
