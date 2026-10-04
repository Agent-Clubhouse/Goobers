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

// EventRouteResult describes one bounded receipt transaction. Found=false is
// idle. A terminal Reason is persisted; returned errors leave custody retryable.
type EventRouteResult struct {
	Found     bool
	ReceiptID string
	State     EventState
	Reason    string
	Groups    []string
}

// RouteNextEvent routes one oldest receipt and at most 32 consumers atomically.
// Serialized selection preserves receipt sequence under concurrent workers.
// No current configuration is consulted; retries cannot rematch subscriptions.
func (s *Store) RouteNextEvent(ctx context.Context, gaggle string, now time.Time) (EventRouteResult, error) {
	if err := validEventBatch(gaggle, now, 1); err != nil {
		return EventRouteResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventRouteResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	receipt, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE gaggle=? AND state='routing_pending' ORDER BY accepted_seq LIMIT 1", gaggle))
	if errors.Is(err, sql.ErrNoRows) {
		return EventRouteResult{}, nil
	}
	if err != nil {
		return EventRouteResult{}, err
	}
	if now.Before(receipt.AcceptedAt) {
		return EventRouteResult{}, errors.New("triggerqueue: routing clock precedes receipt")
	}
	result := EventRouteResult{Found: true, ReceiptID: receipt.ID, State: EventRouted}
	plan, planErr := eventing.ParsePlan(receipt.Plan)
	if !now.Before(receipt.AcceptedAt.Add(EventRoutingDeadline)) {
		result.State, result.Reason = EventRoutingFailed, "routing deadline exceeded"
	} else if planErr != nil {
		result.State, result.Reason = EventRoutingFailed, "routing snapshot lacks valid pinned consumer configuration"
	}
	// Credits move into actual deliveries, open groups and starts inside this
	// transaction. No other intake can observe a temporary release of credit.
	if _, err = tx.ExecContext(ctx, `UPDATE event_receipts SET reserved_bytes=0,reserved_starts=0 WHERE id=?`, receipt.ID); err != nil {
		return EventRouteResult{}, err
	}
	if result.State == EventRouted {
		for _, route := range plan.Routes {
			if route.FailureReason != "" {
				if err := failEventDelivery(ctx, tx, receipt, route.Consumer, route.FailureReason); err != nil {
					return EventRouteResult{}, err
				}
				result.State, result.Reason = EventRoutingPartial, "one or more consumer deliveries failed"
				continue
			}
			group, err := routeEventConsumer(ctx, tx, receipt, route)
			if errors.Is(err, ErrEventChainLimit) {
				if err = failEventDelivery(ctx, tx, receipt, route.Consumer, ErrEventChainLimit.Error()); err != nil {
					return EventRouteResult{}, err
				}
				result.State, result.Reason = EventRoutingPartial, "one or more consumer deliveries failed"
				continue
			}
			if err != nil {
				return EventRouteResult{}, err
			}
			result.Groups = append(result.Groups, group.ID)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE event_receipts SET state=?,reason=?,finished_ns=? WHERE id=?`, result.State, result.Reason, now.UnixNano(), receipt.ID); err != nil {
		return EventRouteResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return EventRouteResult{}, err
	}
	return result, nil
}

func failEventDelivery(ctx context.Context, tx *sql.Tx, receipt EventReceipt, consumer, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO event_deliveries(gaggle,receipt_id,consumer,group_id,receipt_seq,reason) VALUES(?,?,?,'',?,?)`, receipt.Producer.Gaggle, receipt.ID, consumer, receipt.Sequence, reason)
	return err
}

func routeEventConsumer(ctx context.Context, tx *sql.Tx, receipt EventReceipt, route eventing.Route) (EventGroup, error) {
	raw, err := json.Marshal(route)
	if err != nil {
		return EventGroup{}, err
	}
	if len(raw) > 4096 {
		return EventGroup{}, errors.New("triggerqueue: retained route exceeds group bound")
	}
	digest := "sha256:" + childDigest(raw)
	key, mode := "", "all"
	if route.Debounce != nil {
		key, mode = route.Debounce.Key, route.Debounce.InputMode
	}
	group, err := scanEventGroup(tx.QueryRowContext(ctx, "SELECT "+eventGroupColumns+" FROM event_groups WHERE gaggle=? AND consumer=? AND revision=? AND route_digest=? AND debounce_key=? AND state='open'", receipt.Producer.Gaggle, route.Consumer, route.Revision, digest, key))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EventGroup{}, err
	}
	if err == nil && !receipt.AcceptedAt.Before(group.Deadline) {
		if _, err = closeEventGroup(ctx, tx, group, group.Deadline, "deadline"); err != nil {
			return EventGroup{}, err
		}
		err = sql.ErrNoRows
	}
	if errors.Is(err, sql.ErrNoRows) {
		group, err = newEventGroup(ctx, tx, receipt, route, raw, digest, key, mode)
	} else {
		if err = reserveEventRoot(ctx, tx, receipt, group.ID); err != nil {
			return EventGroup{}, err
		}
		group.Count++
		group.SelectedReceipt = receipt.ID
		if receipt.AcceptedAt.After(group.LastAt) {
			group.LastAt = receipt.AcceptedAt
		}
		group.Deadline = eventDeadline(group.FirstAt, group.LastAt, route.Debounce)
		_, err = tx.ExecContext(ctx, `UPDATE event_groups SET event_count=?,selected_receipt=?,last_ns=?,deadline_ns=? WHERE id=?`, group.Count, group.SelectedReceipt, group.LastAt.UnixNano(), group.Deadline.UnixNano(), group.ID)
	}
	if err != nil {
		return EventGroup{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO event_deliveries(gaggle,receipt_id,consumer,group_id,receipt_seq) VALUES(?,?,?,?,?)`, receipt.Producer.Gaggle, receipt.ID, route.Consumer, group.ID, receipt.Sequence); err != nil {
		return EventGroup{}, err
	}
	if route.Debounce == nil {
		return closeEventGroup(ctx, tx, group, receipt.AcceptedAt, "immediate")
	}
	if group.Count >= route.Debounce.MaxEvents {
		return closeEventGroup(ctx, tx, group, receipt.AcceptedAt, "count")
	}
	return group, nil
}

func eventDeadline(first, last time.Time, policy *eventing.Debounce) time.Time {
	if policy == nil {
		return last
	}
	quiet, absolute := last.Add(policy.Window), first.Add(policy.MaxWait)
	if quiet.Before(absolute) {
		return quiet
	}
	return absolute
}

func newEventGroup(ctx context.Context, tx *sql.Tx, receipt EventReceipt, route eventing.Route, raw []byte, digest, key, mode string) (EventGroup, error) {
	if err := triggerSlotCapacity(ctx, tx, 1); err != nil {
		return EventGroup{}, err
	}
	if err := childByteCapacity(ctx, tx, eventGroupAllowance+len(raw)+4096); err != nil {
		return EventGroup{}, err
	}
	group := EventGroup{ID: fmt.Sprintf("group-%x", randomID()), Gaggle: receipt.Producer.Gaggle, State: "open", InputMode: mode, SelectedReceipt: receipt.ID, Route: route, FirstAt: receipt.AcceptedAt, LastAt: receipt.AcceptedAt, Count: 1}
	if err := reserveEventRoot(ctx, tx, receipt, group.ID); err != nil {
		return EventGroup{}, err
	}
	group.Deadline = eventDeadline(group.FirstAt, group.LastAt, route.Debounce)
	_, err := tx.ExecContext(ctx, `INSERT INTO event_groups(id,gaggle,consumer,revision,route,route_digest,debounce_key,input_mode,state,first_ns,last_ns,deadline_ns,event_count,selected_receipt,reserved_bytes,reserved_starts) VALUES(?,?,?,?,?,?,?,?,'open',?,?,?,?,?,?,1)`, group.ID, group.Gaggle, route.Consumer, route.Revision, raw, digest, key, mode, group.FirstAt.UnixNano(), group.LastAt.UnixNano(), group.Deadline.UnixNano(), 1, receipt.ID, eventGroupAllowance)
	return group, err
}

func eventStartEnvelope(group EventGroup) eventing.StartEnvelope {
	route := group.Route
	start := eventing.StartEnvelope{Kind: eventing.StartKind, Gaggle: group.Gaggle, GroupID: group.ID, Consumer: route.Consumer, Revision: route.Revision, Workflow: route.Workflow, WorkflowDigest: route.WorkflowDigest, GooberDigest: route.GooberDigest, ConfigGeneration: route.ConfigGeneration, InputMode: group.InputMode, EventCount: group.Count}
	if group.InputMode == "latest" {
		start.SelectedReceipt = group.SelectedReceipt
	}
	return start
}

func closeEventGroup(ctx context.Context, tx *sql.Tx, group EventGroup, now time.Time, reason string) (EventGroup, error) {
	payload, err := eventStartEnvelope(group).Marshal()
	if err != nil {
		return EventGroup{}, err
	}
	group.AcceptanceID = fmt.Sprintf("trigger-%x", randomID())
	group.State, group.CloseReason, group.ClosedAt = "queued", reason, now.UTC()
	_, err = tx.ExecContext(ctx, `UPDATE event_groups SET state='queued',closed_ns=?,close_reason=?,acceptance_id=?,reserved_bytes=0,reserved_starts=0 WHERE id=? AND state='open'`, now.UnixNano(), reason, group.AcceptanceID, group.ID)
	if err != nil {
		return EventGroup{}, err
	}
	if err = triggerSlotCapacity(ctx, tx, 1); err != nil {
		return EventGroup{}, err
	}
	if err = childByteCapacity(ctx, tx, len(payload)+4096); err != nil {
		return EventGroup{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) VALUES(?,?,?,?,'accepted',?)`, group.AcceptanceID, "event:"+group.ID, eventStartActor(group), payload, now.UnixNano())
	return group, err
}

func eventStartActor(group EventGroup) string {
	return "event-consumer:" + childDigest([]byte(group.Gaggle+"\x00"+group.Route.Consumer))
}

// CloseEventGroups closes due input sets only after every earlier accepted
// receipt has routed. Delayed routing cannot lose an on-time membership.
func (s *Store) CloseEventGroups(ctx context.Context, gaggle string, now time.Time, limit int) ([]EventGroup, error) {
	if err := validEventBatch(gaggle, now, limit); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	ids, err := childPruneIDs(ctx, tx, `SELECT g.id FROM event_groups g WHERE g.gaggle=? AND g.state='open' AND g.deadline_ns<=?
 AND NOT EXISTS(SELECT 1 FROM event_receipts r WHERE r.gaggle=g.gaggle AND r.state='routing_pending' AND r.accepted_ns<=g.deadline_ns)
 ORDER BY g.deadline_ns,g.id LIMIT ?`, gaggle, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	result := make([]EventGroup, 0, len(ids))
	for _, id := range ids {
		group, err := scanEventGroup(tx.QueryRowContext(ctx, "SELECT "+eventGroupColumns+" FROM event_groups WHERE gaggle=? AND id=?", gaggle, id))
		if err != nil {
			return nil, err
		}
		group, err = closeEventGroup(ctx, tx, group, group.Deadline, "deadline")
		if err != nil {
			return nil, err
		}
		result = append(result, group)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
