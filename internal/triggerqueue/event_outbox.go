package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/eventing"
)

// MaxEventPublications bounds retained producer outbox custody per gaggle.
const MaxEventPublications = 10000

const eventOutboxSchema = `
CREATE TABLE event_outbox (
 id TEXT PRIMARY KEY NOT NULL,
 gaggle TEXT NOT NULL, run_id TEXT NOT NULL, generation TEXT NOT NULL,
 occurrence TEXT NOT NULL, envelope_digest TEXT NOT NULL,
 request BLOB NOT NULL CHECK(length(request) BETWEEN 1 AND 65536),
 receipt_id TEXT NOT NULL DEFAULT '', created_ns INTEGER NOT NULL
);
CREATE INDEX event_outbox_scope ON event_outbox(gaggle,run_id,id);
`

// EventPublication is a durable intent, persisted before receipt admission.
// Its original plan survives retries and subscription changes. Completed intents
// remain bounded custody until the host can prove the producer can never retry.
type EventPublication struct {
	ID               string
	ConfigGeneration string
	Occurrence       string
	Acceptance       EventAcceptance
	ReceiptID        string
	CreatedAt        time.Time
	SettledAt        time.Time
	TerminalSequence uint64
	TerminalPhase    string
}

// BeginEventPublication commits immutable intent before publication. Same-key
// retries recover the first plan; changed payload, producer or source pins fail.
func (s *Store) BeginEventPublication(ctx context.Context, req EventPublication, now time.Time) (EventPublication, bool, error) {
	envelope, _, _, err := validateEventAcceptance(req.Acceptance, now)
	if err != nil {
		return EventPublication{}, false, err
	}
	if req.ID != envelope.ID || req.Acceptance.Producer.RunID == "" || !validChildText(req.ConfigGeneration, 256, true) || !validChildText(req.Occurrence, 128, true) {
		return EventPublication{}, false, errors.New("triggerqueue: invalid publication custody")
	}
	req.Acceptance.Envelope = envelope.JSON
	raw, err := json.Marshal(req.Acceptance)
	if err != nil || len(raw) > 65536 {
		return EventPublication{}, false, errors.New("triggerqueue: publication exceeds size bound")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EventPublication{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	old, err := scanEventPublication(tx.QueryRowContext(ctx, "SELECT "+eventOutboxColumns+" FROM event_outbox WHERE id=?", req.ID))
	if err == nil {
		prior, parseErr := eventing.Parse(old.Acceptance.Envelope)
		if parseErr != nil || prior.Digest != envelope.Digest || old.Acceptance.Producer != req.Acceptance.Producer || old.ConfigGeneration != req.ConfigGeneration || old.Occurrence != req.Occurrence {
			return EventPublication{}, false, ErrConflict
		}
		if !old.SettledAt.IsZero() {
			return old, true, ErrEventPublicationSettled
		}
		return old, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EventPublication{}, false, err
	}
	if err = refuseSettledPublication(ctx, tx, req); err != nil {
		return EventPublication{}, false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM event_outbox WHERE gaggle=?", req.Acceptance.Producer.Gaggle).Scan(&count); err != nil {
		return EventPublication{}, false, err
	}
	if count >= MaxEventPublications {
		return EventPublication{}, false, ErrFull
	}
	if err = childByteCapacity(ctx, tx, len(raw)+4096); err != nil {
		return EventPublication{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO event_outbox(id,gaggle,run_id,generation,occurrence,envelope_digest,request,created_ns,source_group) VALUES(?,?,?,?,?,?,?,?,?)`, req.ID, req.Acceptance.Producer.Gaggle, req.Acceptance.Producer.RunID, req.ConfigGeneration, req.Occurrence, envelope.Digest, raw, now.UnixNano(), req.Acceptance.Producer.RootGroupID)
	if err != nil {
		return EventPublication{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return EventPublication{}, false, err
	}
	req.CreatedAt = now.UTC()
	return req, false, nil
}

const eventOutboxColumns = "id,gaggle,run_id,generation,occurrence,envelope_digest,request,receipt_id,created_ns,source_group,settled_ns,terminal_seq,terminal_phase"

func scanEventPublication(row scanner) (EventPublication, error) {
	var p EventPublication
	var gaggle, run, digest, sourceGroup string
	var raw []byte
	var created int64
	var settled, terminalSequence sql.NullInt64
	if err := row.Scan(&p.ID, &gaggle, &run, &p.ConfigGeneration, &p.Occurrence, &digest, &raw, &p.ReceiptID, &created, &sourceGroup, &settled, &terminalSequence, &p.TerminalPhase); err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p.Acceptance); err != nil {
		return p, err
	}
	env, _, _, err := validateEventAcceptance(p.Acceptance, time.Unix(0, created))
	if err != nil {
		return p, err
	}
	if sourceGroup != p.Acceptance.Producer.RootGroupID || env.ID != p.ID || env.Digest != digest || gaggle != p.Acceptance.Producer.Gaggle || run != p.Acceptance.Producer.RunID || run == "" || !validChildText(p.ConfigGeneration, 256, true) || !validChildText(p.Occurrence, 128, true) {
		return p, errors.New("triggerqueue: invalid retained publication")
	}
	p.CreatedAt = time.Unix(0, created).UTC()
	if !settled.Valid && (terminalSequence.Valid || p.TerminalPhase != "") {
		return p, errors.New("triggerqueue: incomplete publication terminal fence")
	}
	if settled.Valid {
		if !terminalSequence.Valid || terminalSequence.Int64 <= 0 || (p.TerminalPhase != "completed" && p.TerminalPhase != "aborted") {
			return p, errors.New("triggerqueue: invalid publication terminal fence")
		}
		p.SettledAt = time.Unix(0, settled.Int64).UTC()
		p.TerminalSequence = uint64(terminalSequence.Int64)
	}
	return p, nil
}

// CompleteEventPublication records an exact accepted receipt. The admission may
// have succeeded before a process died; repeating admission recovers its ID.
func (s *Store) CompleteEventPublication(ctx context.Context, p EventPublication, receipt EventReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanEventPublication(tx.QueryRowContext(ctx, "SELECT "+eventOutboxColumns+" FROM event_outbox WHERE id=?", p.ID))
	if err != nil {
		return err
	}
	actual, err := scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM event_receipts WHERE id=? AND gaggle=? AND producer=?", receipt.ID, p.Acceptance.Producer.Gaggle, p.Acceptance.Producer.Binding))
	if err != nil {
		return err
	}
	env, err := eventing.Parse(stored.Acceptance.Envelope)
	if err != nil {
		return err
	}
	if actual.Producer != stored.Acceptance.Producer || actual.Producer != receipt.Producer || actual.Digest != env.Digest || actual.Digest != receipt.Digest || actual.EventID != p.ID {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE event_outbox SET receipt_id=? WHERE id=? AND (receipt_id='' OR receipt_id=?)`, actual.ID, p.ID, actual.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}

// EventPublicationPage inventories producer journal and generation custody,
// including intents whose intake acknowledgement was lost. No partial scan may
// authorize pruning. Release requires an exact terminal-producer fence.
func (s *Store) EventPublicationPage(ctx context.Context, after string, limit int) ([]EventPublication, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("triggerqueue: invalid publication page")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+eventOutboxColumns+" FROM event_outbox WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EventPublication
	for rows.Next() {
		p, err := scanEventPublication(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
