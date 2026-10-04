package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

// ErrEventPublicationSettled means terminal custody has already closed this
// producer intent. An exact observation is allowed; republication is forbidden.
var ErrEventPublicationSettled = errors.New("triggerqueue: event publication producer settled")

const eventPublicationRetentionSchema = `
ALTER TABLE event_outbox ADD COLUMN source_group TEXT NOT NULL DEFAULT '';
UPDATE event_outbox SET source_group=COALESCE(json_extract(request,'$.Producer.rootGroupId'),'');
CREATE INDEX event_outbox_ancestry ON event_outbox(source_group,id);
ALTER TABLE event_outbox ADD COLUMN settled_ns INTEGER;
ALTER TABLE event_outbox ADD COLUMN terminal_seq INTEGER;
ALTER TABLE event_outbox ADD COLUMN terminal_phase TEXT NOT NULL DEFAULT '';
CREATE INDEX event_outbox_retention ON event_outbox(settled_ns,id);
CREATE TABLE event_publication_tombstones (
 id TEXT PRIMARY KEY NOT NULL, gaggle TEXT NOT NULL, run_id TEXT NOT NULL,
 request_digest TEXT NOT NULL, receipt_id TEXT NOT NULL,
 terminal_seq INTEGER NOT NULL CHECK(terminal_seq>0), terminal_phase TEXT NOT NULL CHECK(terminal_phase IN ('completed','aborted')),
 settled_ns INTEGER NOT NULL, tombstoned_ns INTEGER NOT NULL
);
CREATE INDEX event_publication_tombstones_scope ON event_publication_tombstones(gaggle,tombstoned_ns,id);
`

// EventProducerTerminal is host evidence collected under exclusive runner and
// journal ownership. Failed/escalated/interrupted runs remain resumable and are
// deliberately not accepted here.
type EventProducerTerminal struct {
	Gaggle, RunID, ConfigGeneration, Phase string
	Sequence                               uint64
}

// EventPublicationObservation contains no replayable payload or route plan.
// Exact observations after compaction cannot recreate an intent or publication.
type EventPublicationObservation struct {
	ID, ReceiptID, TerminalPhase string
	TerminalSequence             uint64
	SettledAt, TombstonedAt      time.Time
}

func eventPublicationRequestDigest(p EventPublication) (string, error) {
	env, err := eventing.Parse(p.Acceptance.Envelope)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Producer                       EventProducer
		Generation, Occurrence, Digest string
	}{p.Acceptance.Producer, p.ConfigGeneration, p.Occurrence, env.Digest})
	if err != nil {
		return "", err
	}
	return "sha256:" + childDigest(raw), nil
}

// SettleEventPublication fences one intent using the exact non-resumable
// terminal source. It also recovers a receipt whose acceptance reply was lost.
func (s *Store) SettleEventPublication(ctx context.Context, id string, proof EventProducerTerminal, now time.Time) error {
	if proof.Sequence == 0 || proof.Sequence > math.MaxInt64 || (proof.Phase != "completed" && proof.Phase != "aborted") || now.IsZero() {
		return ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := scanEventPublication(tx.QueryRowContext(ctx, "SELECT "+eventOutboxColumns+" FROM event_outbox WHERE id=?", id))
	if err != nil {
		return err
	}
	if p.Acceptance.Producer.Gaggle != proof.Gaggle || p.Acceptance.Producer.RunID != proof.RunID || p.ConfigGeneration != proof.ConfigGeneration || now.Before(p.CreatedAt) {
		return ErrConflict
	}
	if !p.SettledAt.IsZero() {
		if p.TerminalSequence != proof.Sequence || p.TerminalPhase != proof.Phase {
			return ErrConflict
		}
		return nil
	}
	env, err := eventing.Parse(p.Acceptance.Envelope)
	if err != nil {
		return err
	}
	receipt, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE gaggle=? AND producer=? AND source=? AND event_id=?", proof.Gaggle, p.Acceptance.Producer.Binding, env.Source, env.ID))
	if err == nil {
		if receipt.Producer != p.Acceptance.Producer || receipt.Digest != env.Digest {
			return ErrConflict
		}
		p.ReceiptID = receipt.ID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else if p.ReceiptID != "" {
		return errors.New("triggerqueue: settled publication receipt missing")
	}
	_, err = tx.ExecContext(ctx, `UPDATE event_outbox SET receipt_id=?,settled_ns=?,terminal_seq=?,terminal_phase=? WHERE id=? AND settled_ns IS NULL`, p.ReceiptID, now.UnixNano(), proof.Sequence, proof.Phase, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ObserveEventPublication is an exact-request read for a previously authorized
// host caller. Compacted records cannot be used to resume or re-admit an event.
func (s *Store) ObserveEventPublication(ctx context.Context, req EventPublication) (EventPublicationObservation, error) {
	digest, err := eventPublicationRequestDigest(req)
	if err != nil {
		return EventPublicationObservation{}, err
	}
	return observePublicationTombstone(s.db.QueryRowContext(ctx, `SELECT id,request_digest,receipt_id,terminal_seq,terminal_phase,settled_ns,tombstoned_ns FROM event_publication_tombstones WHERE id=? AND gaggle=? AND run_id=?`, req.ID, req.Acceptance.Producer.Gaggle, req.Acceptance.Producer.RunID), digest)
}

func observePublicationTombstone(row scanner, expected string) (EventPublicationObservation, error) {
	var result EventPublicationObservation
	var digest string
	var settled, tombstoned int64
	err := row.Scan(&result.ID, &digest, &result.ReceiptID, &result.TerminalSequence, &result.TerminalPhase, &settled, &tombstoned)
	if err != nil {
		return result, err
	}
	if digest != expected {
		return result, ErrConflict
	}
	if result.TerminalSequence == 0 || (result.TerminalPhase != "completed" && result.TerminalPhase != "aborted") {
		return result, errors.New("triggerqueue: invalid publication tombstone")
	}
	result.SettledAt = time.Unix(0, settled).UTC()
	result.TombstonedAt = time.Unix(0, tombstoned).UTC()
	return result, nil
}

func refuseSettledPublication(ctx context.Context, tx *sql.Tx, req EventPublication) error {
	digest, err := eventPublicationRequestDigest(req)
	if err != nil {
		return err
	}
	_, err = observePublicationTombstone(tx.QueryRowContext(ctx, `SELECT id,request_digest,receipt_id,terminal_seq,terminal_phase,settled_ns,tombstoned_ns FROM event_publication_tombstones WHERE id=?`, req.ID), digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrEventPublicationSettled
}

// PruneEventPublications compacts only exclusively fenced terminal producers
// after the seven-day replay window. Tombstones retain exact request identity
// for thirty more days. One maintenance budget bounds both transitions.
func (s *Store) PruneEventPublications(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return 0, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM event_publication_tombstones WHERE id IN(SELECT id FROM event_publication_tombstones WHERE tombstoned_ns<=? ORDER BY tombstoned_ns,id LIMIT ?)`, now.Add(-EventTombstoneRetention).UnixNano(), max(1, limit/2))
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	ids, err := childPruneIDs(ctx, tx, `SELECT o.id FROM event_outbox o WHERE o.settled_ns<=? AND (SELECT COUNT(*) FROM event_publication_tombstones t WHERE t.gaggle=o.gaggle)<? ORDER BY o.settled_ns,o.id LIMIT ?`, now.Add(-EventRetention).UnixNano(), MaxEventTombstones, limit-int(deleted))
	if err != nil {
		return 0, err
	}
	compacted := 0
	for _, id := range ids {
		changed, compactErr := compactEventPublication(ctx, tx, id, now)
		if compactErr != nil {
			return 0, compactErr
		}
		if changed {
			compacted++
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int(deleted) + compacted, nil
}

func compactEventPublication(ctx context.Context, tx *sql.Tx, id string, now time.Time) (bool, error) {
	p, err := scanEventPublication(tx.QueryRowContext(ctx, "SELECT "+eventOutboxColumns+" FROM event_outbox WHERE id=?", id))
	if err != nil {
		return false, err
	}
	digest, err := eventPublicationRequestDigest(p)
	if err != nil {
		return false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_publication_tombstones WHERE gaggle=?", p.Acceptance.Producer.Gaggle).Scan(&count); err != nil {
		return false, err
	}
	if count >= MaxEventTombstones {
		return false, nil
	}
	// The retained outbox row already reserves more space than its tombstone.
	_, err = tx.ExecContext(ctx, `INSERT INTO event_publication_tombstones(id,gaggle,run_id,request_digest,receipt_id,terminal_seq,terminal_phase,settled_ns,tombstoned_ns) VALUES(?,?,?,?,?,?,?,?,?)`, p.ID, p.Acceptance.Producer.Gaggle, p.Acceptance.Producer.RunID, digest, p.ReceiptID, p.TerminalSequence, p.TerminalPhase, p.SettledAt.UnixNano(), now.UnixNano())
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM event_outbox WHERE id=?", id)
	return err == nil, err
}
