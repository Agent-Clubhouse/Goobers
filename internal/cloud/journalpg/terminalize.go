package journalpg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/journal"
)

const (
	runStalledErrorCode          = "run_stalled"
	runDurationExceededErrorCode = "run_duration_exceeded"
)

// Terminalize durably closes a server-terminated engine run under the live
// control fence, then invokes idempotent external cleanup.
func (b *Backend) Terminalize(
	ctx context.Context,
	identity journal.RunIdentity,
	reason string,
) error {
	handle, err := b.liveOwner()
	if err != nil {
		return err
	}
	err = b.config.Lease.WithFence(ctx, handle,
		func(txCtx context.Context, tx pgx.Tx) error {
			var terminal bool
			var seq int64
			err := tx.QueryRow(txCtx, `
				SELECT checkpoint.terminal,sequence.last_seq
				FROM journal.run_seq AS sequence
				JOIN journal.run_checkpoint AS checkpoint
				  USING (instance_id,run_id)
				WHERE sequence.instance_id=$1 AND sequence.run_id=$2
				FOR UPDATE OF sequence,checkpoint`,
				b.config.InstanceID, identity.RunID,
			).Scan(&terminal, &seq)
			if errors.Is(err, pgx.ErrNoRows) {
				return journal.ErrClosed
			}
			if err != nil || terminal {
				return err
			}
			var at time.Time
			if err := tx.QueryRow(txCtx, `
				UPDATE journal.run_seq
				SET last_seq=last_seq+1,last_append_at=clock_timestamp(),
				    owner_token=NULL,owner_epoch=$3
				WHERE instance_id=$1 AND run_id=$2 AND owner_epoch<=$3
				RETURNING last_seq,last_append_at`,
				b.config.InstanceID, identity.RunID, handle.Epoch,
			).Scan(&seq, &at); err != nil {
				return err
			}
			phase := journal.PhaseEscalated
			code := runStalledErrorCode
			message := "engine run terminated after exceeding the stall timeout"
			if reason == "max_duration" {
				phase = journal.PhaseAborted
				code = runDurationExceededErrorCode
				message = "engine run terminated after exceeding maxRunDuration"
			}
			event := journal.Event{
				Schema: journal.EventSchema, Seq: uint64(seq),
				Time: at.UTC(), Type: journal.EventRunFinished,
				Gaggle: identity.Gaggle, Workflow: identity.Workflow,
				RunID: identity.RunID, Status: string(phase),
				Error: &journal.ErrorDetail{Code: code, Message: message},
			}
			raw, err := journal.MarshalEvent(event)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(txCtx, `
				INSERT INTO journal.run_event (
					instance_id,run_id,seq,type,at,body
				) VALUES ($1,$2,$3,$4,$5,$6)`,
				b.config.InstanceID, identity.RunID, seq,
				string(event.Type), event.Time, raw,
			); err != nil {
				return err
			}
			tag, err := tx.Exec(txCtx, `
				UPDATE journal.run_checkpoint
				SET phase=$3,terminal=true,at_seq=$4
				WHERE instance_id=$1 AND run_id=$2 AND NOT terminal`,
				b.config.InstanceID, identity.RunID, phase, seq,
			)
			return requireWrite(tag.RowsAffected(), err)
		})
	if err != nil {
		return err
	}
	select {
	case b.dirty <- struct{}{}:
	default:
	}
	if b.config.TerminalCleanup != nil {
		return b.config.TerminalCleanup(ctx, identity, reason)
	}
	return nil
}
