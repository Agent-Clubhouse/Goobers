package journalpg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

const maxEventBytes = 8 << 20

// prepareEvent is deliberately called before BEGIN/lease.WithFence. The
// transaction only stamps server-allocated sequencing metadata onto clean data.
func prepareEvent(r *runOwner, event journal.Event) (journal.Event, error) {
	event.Schema = journal.EventSchema
	event.Seq = 1
	event.Time = r.options.Clock()
	if event.Progress != nil {
		progress := *event.Progress
		event.Progress = &progress
		progress.Sequence = 1
		if progress.OccurredAt.IsZero() {
			progress.OccurredAt = event.Time
		}
		if progress.UpdatedAt.IsZero() {
			progress.UpdatedAt = progress.OccurredAt
		}
	}
	raw, err := journal.MarshalEvent(event)
	if err != nil {
		return journal.Event{}, err
	}
	raw = r.options.Scrubber.Scrub(raw)
	if len(raw)+1 > maxEventBytes {
		return journal.Event{}, errors.New("journalpg: event exceeds 8 MiB")
	}
	var clean journal.Event
	if err := json.Unmarshal(raw, &clean); err != nil {
		return clean, fmt.Errorf("journalpg: scrubbed event: %w", err)
	}
	if _, err := journal.MarshalEvent(clean); err != nil {
		return clean, err
	}
	return clean, nil
}
func emitKey(event journal.Event) any {
	key, _ := event.Runner["emitKey"].(string)
	if key == "" {
		return nil
	}
	return key
}

// AppendBatch commits all events or none. Allocation, deduplication, canonical
// bytes and phase checkpoints share one transaction; rollback cannot leave gaps.
func (b *Backend) AppendBatch(ctx context.Context, handle journal.RunHandle, token journal.OwnerToken, events []journal.Event) (int, error) {
	r, err := b.writeRun(handle, token)
	if err != nil {
		return 0, err
	}
	clean := make([]journal.Event, len(events))
	for i, event := range events {
		clean[i], err = prepareEvent(r, event)
		if err != nil {
			return 0, err
		}
	}
	var last int64
	err = b.withAppend(ctx, r, clean, func(ctx context.Context, tx pgx.Tx, seq int64) ([]journal.Event, error) {
		last = seq
		committed := make([]journal.Event, 0, len(clean))
		for _, event := range clean {
			var err error
			var stored journal.Event
			last, stored, err = b.appendClean(ctx, tx, r, last, event)
			if err != nil {
				return nil, err
			}
			committed = append(committed, stored)
		}
		return committed, nil
	})
	if err != nil {
		return 0, err
	}
	if len(events) > 0 {
		b.changed(r, uint64(last))
	}
	return len(events), nil
}

func (b *Backend) appendClean(ctx context.Context, tx pgx.Tx, r *runOwner, seq int64, event journal.Event) (int64, journal.Event, error) {
	if key := emitKey(event); key != nil {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT body FROM journal.run_event WHERE instance_id=$1 AND run_id=$2 AND emit_key=$3`, b.config.InstanceID, r.key.RunID, key).Scan(&raw)
		if err == nil {
			var prior journal.Event
			err = json.Unmarshal(raw, &prior)
			return seq, prior, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return seq, journal.Event{}, err
		}
	}
	if event.Type == journal.EventAgentProgress && event.Progress != nil {
		if err := b.validateProgress(ctx, tx, r, event); err != nil {
			return seq, journal.Event{}, err
		}
	}
	var allocated int64
	if err := tx.QueryRow(ctx, `UPDATE journal.run_seq SET last_seq=last_seq+1,last_append_at=clock_timestamp() WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4 RETURNING last_seq`, b.config.InstanceID, r.key.RunID, r.token, r.epoch).Scan(&allocated); err != nil {
		return seq, journal.Event{}, err
	}
	event.Seq = uint64(allocated)
	if event.Progress != nil {
		event.Progress.Sequence = event.Seq
	}
	raw, err := journal.MarshalEvent(event)
	if err != nil {
		return seq, journal.Event{}, err
	}
	if len(raw)+1 > maxEventBytes {
		return seq, journal.Event{}, errors.New("journalpg: event exceeds 8 MiB")
	}
	notificationKey, notificationSink, notificationAttempt := notificationIndex(event)
	agentID, agentRun, agentStage, agentAttempt := progressIndex(event)
	tag, err := tx.Exec(ctx, `INSERT INTO journal.run_event(instance_id,run_id,seq,branch,type,stage,attempt,emit_key,at,body,operator_key,notification_key,notification_sink,notification_attempt,agent_id,agent_run,agent_stage,agent_attempt,at_submicro)
 SELECT instance_id,run_id,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21 FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4`,
		b.config.InstanceID, r.key.RunID, r.token, r.epoch, int64(event.Seq), event.Branch, string(event.Type), event.Stage, event.Attempt, emitKey(event), event.Time.Truncate(time.Microsecond), raw, operatorKey(event), notificationKey, notificationSink, notificationAttempt, agentID, agentRun, agentStage, agentAttempt, event.Time.Nanosecond()%1000)
	if err = requireWrite(tag.RowsAffected(), err); err != nil {
		return seq, journal.Event{}, err
	}

	if err = b.indexArtifact(ctx, tx, r, event); err != nil {
		return seq, journal.Event{}, err
	}
	return int64(event.Seq), event, err
}
func lifecycle(event journal.Event) bool {
	switch event.Type {
	case journal.EventRunStarted, journal.EventRunResumed, journal.EventRunFinished, journal.EventGateOverridden, journal.EventGateEvaluated, journal.EventStageRerunRequested:
		return true
	default:
		return false
	}
}
func (b *Backend) indexArtifact(ctx context.Context, tx pgx.Tx, r *runOwner, event journal.Event) error {
	if event.Ref == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO journal.run_artifact(instance_id,run_id,seq,digest,kind)
 SELECT instance_id,run_id,$5,$6,$7 FROM journal.run_seq WHERE instance_id=$1 AND run_id=$2 AND owner_token=$3 AND owner_epoch=$4`, b.config.InstanceID, r.key.RunID, r.token, r.epoch, int64(event.Seq), event.Ref.Digest, string(event.Type))
	return requireWrite(tag.RowsAffected(), err)
}

// AppendIfAbsent evaluates a pure predicate over the snapshot outside the
// fence, then over only the concurrent tail while holding the append lock.
func (b *Backend) AppendIfAbsent(ctx context.Context, h journal.RunHandle, o journal.OwnerToken, event journal.Event, match func(journal.Event) bool) (bool, error) {
	r, err := b.writeRun(h, o)
	if err != nil {
		return false, err
	}
	if match == nil {
		return false, errors.New("journalpg: match predicate required")
	}
	clean, err := prepareEvent(r, event)
	if err != nil {
		return false, err
	}
	appended := false
	var last int64
	err = b.withHistory(ctx, r, func(s historySnapshot) (tailWrite, error) {
		matched := matchesHistory(s.events, match)
		var replay *replayState
		if lifecycle(clean) {
			replay = newReplayState(r.key.RunID, s.events)
		}
		return func(ctx context.Context, tx pgx.Tx, seq int64, tail []journal.Event) error {
			if matched || matchesHistory(tail, match) {
				return nil
			}
			var stored journal.Event
			var err error
			last, stored, err = b.appendClean(ctx, tx, r, seq, clean)
			if err != nil {
				return err
			}
			appended = true
			if replay == nil {
				return nil
			}
			replay.fold(tail)
			replay.fold([]journal.Event{stored})
			checkpoint, err := replay.checkpoint()
			if err != nil {
				return err
			}
			return b.storeCheckpoint(ctx, tx, r, checkpoint)
		}, nil
	})
	if err != nil {
		return false, err
	}
	if appended {
		b.changed(r, uint64(last))
	}
	return appended, nil
}

// Checkpoint refreshes the derived state; committed event bytes remain truth.
func (b *Backend) Checkpoint(ctx context.Context, h journal.RunHandle, o journal.OwnerToken) error {
	r, err := b.writeRun(h, o)
	if err != nil {
		return err
	}
	return b.withHistory(ctx, r, func(s historySnapshot) (tailWrite, error) {
		replay := newReplayState(r.key.RunID, s.events)
		return func(ctx context.Context, tx pgx.Tx, _ int64, tail []journal.Event) error {
			replay.fold(tail)
			checkpoint, err := replay.checkpoint()
			if err != nil {
				return err
			}
			return b.storeCheckpoint(ctx, tx, r, checkpoint)
		}, nil
	})
}

// RepairAppendBoundary validates ownership. Atomic commits leave no torn tail.
func (b *Backend) RepairAppendBoundary(ctx context.Context, h journal.RunHandle, o journal.OwnerToken) error {
	r, err := b.writeRun(h, o)
	if err != nil {
		return err
	}
	return b.withRun(ctx, r, func(context.Context, pgx.Tx, int64) error { return nil })
}

// RunNotifications coalesces wakeups outside emit transactions on a dedicated
// connection. Consumers must still poll at least once per second: notifications
// are hints and a process crash between commit and wakeup may lose one.
func (b *Backend) RunNotifications(ctx context.Context, conn *pgx.Conn) error {
	if conn == nil {
		return errors.New("journalpg: dedicated notification connection required")
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			select {
			case <-b.dirty:
				call, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, err := conn.Exec(call, `SELECT pg_notify('goobers_journal',$1)`, b.config.InstanceID)
				cancel()
				if err != nil {
					return err
				}
			default:
			}
		}
	}
}

func matchesHistory(events []journal.Event, match func(journal.Event) bool) bool {
	for _, event := range events {
		if match(event) {
			return true
		}
	}
	return false
}
