package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

// EventMember identifies an original receipt. Latest mode preserves all members;
// Selected tells the consumer which payload supplies its effective input.
type EventMember struct {
	ReceiptID, Digest string
	Sequence          int64
	Selected          bool
	Producer          EventProducer
}

// EventDelivery preserves each consumer's success or permanent refusal.
type EventDelivery struct{ Consumer, GroupID, Reason string }

// EventDeliveries returns at most MaxConsumers entries for a scoped receipt.
func (s *Store) EventDeliveries(ctx context.Context, gaggle, receiptID string) ([]EventDelivery, error) {
	var found int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_receipts WHERE gaggle=? AND id=?`, gaggle, receiptID).Scan(&found); err != nil {
		return nil, err
	}
	if found != 1 {
		return nil, sql.ErrNoRows
	}
	rows, err := s.db.QueryContext(ctx, `SELECT consumer,group_id,reason FROM event_deliveries WHERE gaggle=? AND receipt_id=? ORDER BY consumer LIMIT ?`, gaggle, receiptID, eventing.MaxConsumers)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EventDelivery
	for rows.Next() {
		var d EventDelivery
		if err := rows.Scan(&d.Consumer, &d.GroupID, &d.Reason); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// EventMembers pages ordered references; it never returns unbounded inline data.
func (s *Store) EventMembers(ctx context.Context, gaggle, groupID string, after int64, limit int) ([]EventMember, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid event membership page")
	}
	group, err := s.EventGroup(ctx, gaggle, groupID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.receipt_id,r.digest,d.receipt_seq,r.authority FROM event_deliveries d JOIN event_receipts r ON r.id=d.receipt_id
 WHERE d.gaggle=? AND d.group_id=? AND d.receipt_seq>? ORDER BY d.receipt_seq,d.receipt_id LIMIT ?`, gaggle, groupID, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EventMember
	for rows.Next() {
		var member EventMember
		var authority []byte
		if err := rows.Scan(&member.ReceiptID, &member.Digest, &member.Sequence, &authority); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(authority, &member.Producer); err != nil || !validEventProducer(member.Producer) {
			return nil, errors.New("triggerqueue: invalid event member authority")
		}
		member.Selected = group.InputMode == "all" || member.ReceiptID == group.SelectedReceipt
		result = append(result, member)
	}
	return result, rows.Err()
}

// EventInput requires both gaggle ownership and actual group membership. A
// guessed receipt ID or digest cannot authorize hydration from another source.
func (s *Store) EventInput(ctx context.Context, gaggle, groupID, receiptID string) (EventReceipt, error) {
	var found int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_deliveries d JOIN event_groups g ON g.id=d.group_id WHERE d.gaggle=? AND g.gaggle=? AND d.group_id=? AND d.receipt_id=?`, gaggle, gaggle, groupID, receiptID).Scan(&found); err != nil {
		return EventReceipt{}, err
	}
	if found != 1 {
		return EventReceipt{}, sql.ErrNoRows
	}
	return scanEvent(s.db.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE gaggle=? AND id=? AND tombstoned_ns IS NULL", gaggle, receiptID))
}

// VerifiedEventStart proves queued bytes still equal the immutable closed group.
// Launchers must additionally acquire the generation pin and current authority.
func (s *Store) VerifiedEventStart(ctx context.Context, gaggle, groupID string) (Record, eventing.StartEnvelope, error) {
	group, err := s.EventGroup(ctx, gaggle, groupID)
	if err != nil {
		return Record{}, eventing.StartEnvelope{}, err
	}
	if group.State != "queued" {
		return Record{}, eventing.StartEnvelope{}, ErrTransition
	}
	record, err := s.Get(ctx, group.AcceptanceID, eventStartActor(group))
	if err != nil {
		return Record{}, eventing.StartEnvelope{}, err
	}
	want := eventStartEnvelope(group)
	if err := verifyEventStartRecord(group, record); err != nil {
		return Record{}, eventing.StartEnvelope{}, err
	}
	return record, want, nil
}

func verifyEventStartRecord(group EventGroup, record Record) error {
	raw, err := eventStartEnvelope(group).Marshal()
	if err != nil {
		return err
	}
	if record.ID != group.AcceptanceID || record.Actor != eventStartActor(group) || record.Key != "event:"+group.ID || string(record.Payload) != string(raw) {
		return ErrConflict
	}
	return nil
}

// SettleEventGroup is called only after the host verifies a terminal consumer
// journal (or durable start rejection). Queue dispatch acknowledgement alone is
// not settlement. Repeated identical observation is idempotent.
func (s *Store) SettleEventGroup(ctx context.Context, gaggle, groupID, expectedRunID, outcome string, now time.Time) error {
	if err := validEventBatch(gaggle, now, 1); err != nil {
		return err
	}
	if outcome != "completed" && outcome != "failed" && outcome != "cancelled" && outcome != "rejected" {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	group, err := scanEventGroup(tx.QueryRowContext(ctx, "SELECT "+eventGroupColumns+" FROM event_groups WHERE gaggle=? AND id=?", gaggle, groupID))
	if err != nil {
		return err
	}
	record, err := scanRecord(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM triggers WHERE id=?", group.AcceptanceID))
	if err != nil {
		return err
	}
	if err = verifyEventStartRecord(group, record); err != nil {
		return err
	}
	if !validEventSettlement(group, record, expectedRunID, outcome, now) {
		return ErrTransition
	}
	if !group.SettledAt.IsZero() {
		if group.Outcome != outcome {
			return ErrConflict
		}
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE event_groups SET settled_ns=?,outcome=? WHERE gaggle=? AND id=? AND settled_ns IS NULL`, now.UnixNano(), outcome, gaggle, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

func validEventSettlement(group EventGroup, record Record, runID, outcome string, now time.Time) bool {
	if group.State != "queued" || now.Before(group.ClosedAt) {
		return false
	}
	if outcome == "rejected" {
		return record.State == Rejected && record.RunID == "" && runID == ""
	}
	return record.State == Dispatched && record.RunID == runID && runID == strings.TrimPrefix(group.AcceptanceID, "trigger-")
}

// EventDependency identifies archival custody before routing as well as while
// starts are queued/running. This is a daemon retention inventory, not a human
// read API; generations and source/consumer journals must remain pinned.
type EventDependency struct {
	ReceiptID, Gaggle, SourceRunID    string
	ConfigGenerations, ConsumerRunIDs []string
}

// EventDependencyPage is bounded and fails closed on corrupt retained pins.
// Callers must finish all pages successfully before pruning any dependencies.
func (s *Store) EventDependencyPage(ctx context.Context, after string, limit int) ([]EventDependency, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid event dependency page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE tombstoned_ns IS NULL AND id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	var receipts []EventReceipt
	for rows.Next() {
		r, err := scanEvent(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		receipts = append(receipts, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var result []EventDependency
	for _, receipt := range receipts {
		pin, err := s.eventDependencies(ctx, receipt)
		if err != nil {
			return nil, err
		}
		result = append(result, pin)
	}
	return result, nil
}

func (s *Store) eventDependencies(ctx context.Context, receipt EventReceipt) (EventDependency, error) {
	result := EventDependency{ReceiptID: receipt.ID, Gaggle: receipt.Producer.Gaggle, SourceRunID: receipt.Producer.RunID}
	plan, err := eventing.ParsePlan(receipt.Plan)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, route := range plan.Routes {
		if !seen[route.ConfigGeneration] {
			result.ConfigGenerations = append(result.ConfigGenerations, route.ConfigGeneration)
			seen[route.ConfigGeneration] = true
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT g.acceptance_id FROM event_deliveries d JOIN event_groups g ON g.id=d.group_id WHERE d.gaggle=? AND d.receipt_id=? AND g.acceptance_id<>'' ORDER BY g.acceptance_id`, result.Gaggle, receipt.ID)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return result, err
		}
		result.ConsumerRunIDs = append(result.ConsumerRunIDs, strings.TrimPrefix(id, "trigger-"))
	}
	return result, rows.Err()
}
