package readmodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The change feed (#1919, design §4.2).
//
// A change row is the PROJECTION position — the ordering key and SSE cursor —
// as distinct from the source position (runID, journalSeq) a writer can promise
// immediately. §15.3 answers "what is the durable unit of change?" with "two,
// deliberately", and conflating them is the mistake the second design pass made.
//
// The two cannot be one number. A mutation commits the journal and returns
// (runID, journalSeq); the projector allocates change.seq asynchronously,
// *after* the mutation returned, so at response time that value does not exist.
// And a projection sequence cannot reveal a source append the projector has not
// discovered yet — it would report zero lag while being arbitrarily behind.

// ChangeKind names a projected transition.
type ChangeKind string

// The projected transitions a client can act on. Distinguished rather than
// collapsed into one "changed" because they call for different client
// behaviour: a creation may need a list prepend while a progression only patches
// a row in place — which is what stops an arriving event from discarding a
// user's pagination (#1713).
const (
	// ChangeRunCreated is a run projected for the first time.
	ChangeRunCreated ChangeKind = "run.created"
	// ChangeRunProgressed is an advance within a run that is still active.
	ChangeRunProgressed ChangeKind = "run.progressed"
	// ChangeRunFinished is a run reaching a terminal phase.
	ChangeRunFinished ChangeKind = "run.finished"
	// ChangeDefinitionsChanged is a config reload: the workflow definitions
	// themselves changed, not any run.
	//
	// It carries no run id. Added when the filesystem poller was deleted
	// (#1929): that detector had its own out-of-band PublishDefinitionsChanged,
	// and dropping it without an equivalent here would have silently stopped the
	// portal noticing config reloads. Routing it through the same ordered feed
	// is the point — a second publish path is exactly the split §8.1 removes.
	ChangeDefinitionsChanged ChangeKind = "definitions.changed"
	// ChangeRunRemoved is a run whose journal has gone and whose rows were
	// deleted. Emitted by the ordered removal protocol and by bidirectional
	// repair (§4.3, §6.3), neither of which exists yet.
	ChangeRunRemoved ChangeKind = "run.removed"
)

// Change is one projected transition.
type Change struct {
	Seq      uint64
	At       time.Time
	Kind     ChangeKind
	RunID    string
	Gaggle   string
	Workflow string
}

// Cursor is a client's position in the change feed.
//
// It carries the schema version and epoch alongside the sequence because a bare
// sequence is ambiguous across rebuilds: a new store restarts numbering, so the
// same integer means different things in different generations.
type Cursor struct {
	SchemaVersion int
	Epoch         string
	Seq           uint64
}

// String renders the wire form, <schemaVersion>:<epoch>:<seq>.
func (c Cursor) String() string {
	return strconv.Itoa(c.SchemaVersion) + ":" + c.Epoch + ":" + strconv.FormatUint(c.Seq, 10)
}

// ParseCursor parses the wire form.
func ParseCursor(raw string) (Cursor, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return Cursor{}, fmt.Errorf("readmodel: malformed cursor %q", raw)
	}
	version, err := strconv.Atoi(parts[0])
	if err != nil {
		return Cursor{}, fmt.Errorf("readmodel: malformed cursor schema version in %q: %w", raw, err)
	}
	seq, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("readmodel: malformed cursor sequence in %q: %w", raw, err)
	}
	if parts[1] == "" {
		return Cursor{}, fmt.Errorf("readmodel: cursor %q has no epoch", raw)
	}
	return Cursor{SchemaVersion: version, Epoch: parts[1], Seq: seq}, nil
}

// CursorCondition is why a cursor cannot be resumed from.
//
// Three NAMED conditions rather than one generic "stale cursor" (§8.1). The
// distinction is not pedantry: they need different client responses, and a
// single code forces the client to guess. Today's stream has exactly this
// problem — ErrStaleEventCursor covers two unrelated causes.
type CursorCondition string

const (
	// CursorOK means the cursor is resumable.
	CursorOK CursorCondition = ""
	// CursorEpochChanged means the store was rebuilt. Equality, not ordering
	// (§4.2): any inequality is a new generation, and the client must refetch a
	// snapshot rather than discard the response — §8.2's discard rule would
	// otherwise make the staleness permanent.
	CursorEpochChanged CursorCondition = "epoch_changed"
	// CursorFeedTruncated means the cursor is below the retention floor: a
	// persisted comparison against min_change_seq, not "roughly old".
	CursorFeedTruncated CursorCondition = "feed_truncated"
	// CursorSchemaChanged means the client's understanding of the feed predates
	// this schema.
	CursorSchemaChanged CursorCondition = "schema_changed"
)

// EvaluateCursor reports whether a cursor can be resumed from.
//
// Order matters. Schema is checked first because a schema change subsumes the
// others; epoch before truncation because comparing a foreign epoch's sequence
// to this store's floor is meaningless — the numbers come from different
// generations and any conclusion drawn from them is an accident.
func EvaluateCursor(cursor Cursor, state State) CursorCondition {
	switch {
	case cursor.SchemaVersion != state.SchemaVersion:
		return CursorSchemaChanged
	case cursor.Epoch != state.Epoch:
		return CursorEpochChanged
	case cursor.Seq < state.MinChangeSeq:
		return CursorFeedTruncated
	default:
		return CursorOK
	}
}

// appendChange writes a change row inside an existing transaction.
//
// Unexported and transaction-scoped on purpose: §6.2 requires the change row to
// commit with the run row it describes, so there is deliberately no way to write
// one on its own. A change published before its fact would tell a client to
// refetch data that is not there yet.
func appendChange(ctx context.Context, tx *sql.Tx, at time.Time, kind ChangeKind, row RunRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO change (at, kind, run_id, gaggle, workflow)
		VALUES (?, ?, ?, ?, ?)`,
		formatTime(at), string(kind), row.RunID, nullString(row.Gaggle), nullString(row.Workflow))
	if err != nil {
		return fmt.Errorf("readmodel: append change for %s: %w", row.RunID, err)
	}
	return nil
}

// changeKindFor classifies a projection against what was previously stored.
//
// created / progressed / finished are distinguished because a client can act on
// them differently — a new run may need a list prepend while a progression only
// patches a row in place (§8.2's in-place patching, which is what stops an
// arriving event from discarding a user's pagination, #1713).
func changeKindFor(previous RunRow, existed bool, next RunRow) ChangeKind {
	switch {
	case !existed:
		return ChangeRunCreated
	case next.Terminal && !previous.Terminal:
		return ChangeRunFinished
	default:
		return ChangeRunProgressed
	}
}

// Changes reads change rows after a cursor position, oldest first.
//
// limit bounds the page: an SSE client resuming after a disconnect must not pull
// an unbounded backlog into memory in one read.
func (s *Store) Changes(ctx context.Context, afterSeq uint64, limit int) ([]Change, error) {
	if limit <= 0 {
		limit = defaultChangePageSize
	}
	db, release, err := s.readHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.QueryContext(ctx, `
		SELECT seq, at, kind, run_id, gaggle, workflow
		FROM change WHERE seq > ? ORDER BY seq ASC LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("readmodel: read changes after %d: %w", afterSeq, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Change
	for rows.Next() {
		var (
			c                       Change
			at                      string
			runID, gaggle, workflow sql.NullString
			kind                    string
		)
		if err := rows.Scan(&c.Seq, &at, &kind, &runID, &gaggle, &workflow); err != nil {
			return nil, fmt.Errorf("readmodel: scan change: %w", err)
		}
		parsed, err := time.Parse(timeFormat, at)
		if err != nil {
			return nil, fmt.Errorf("readmodel: parse change timestamp %q: %w", at, err)
		}
		c.At, c.Kind = parsed, ChangeKind(kind)
		c.RunID, c.Gaggle, c.Workflow = runID.String, gaggle.String, workflow.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readmodel: change rows: %w", err)
	}
	return out, nil
}

// defaultChangePageSize bounds one change read.
const defaultChangePageSize = 500

// LatestChangeSeq returns the highest change sequence, or 0 for an empty feed.
func (s *Store) LatestChangeSeq(ctx context.Context) (uint64, error) {
	var seq sql.NullInt64
	db, release, err := s.readHandle()
	if err != nil {
		return 0, err
	}
	defer release()
	err = db.QueryRowContext(ctx, `SELECT MAX(seq) FROM change`).Scan(&seq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("readmodel: read latest change seq: %w", err)
	}
	if !seq.Valid || seq.Int64 < 0 {
		return 0, nil
	}
	return uint64(seq.Int64), nil
}

// PruneChanges deletes change rows below a floor and advances min_change_seq in
// the SAME transaction as the delete.
//
// Together, because §4.2 requires the floor to be a persisted fact rather than
// an implication: `cursor.seq < min_change_seq` is an exact condition, and it is
// only exact if the recorded floor can never disagree with what is actually in
// the table. Advancing it separately would leave a window where a client's
// cursor is judged resumable against rows that have already gone.
//
// keepFrom is the oldest sequence to retain. Rows below it are removed.
func (s *Store) PruneChanges(ctx context.Context, keepFrom uint64) (int64, error) {
	db, release, err := s.writeHandle()
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("readmodel: begin change prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM change WHERE seq < ?`, keepFrom)
	if err != nil {
		return 0, fmt.Errorf("readmodel: prune changes below %d: %w", keepFrom, err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("readmodel: prune changes: %w", err)
	}
	// The floor only ever advances. A pruner that lowered it would declare
	// already-deleted rows resumable.
	if _, err := tx.ExecContext(ctx,
		`UPDATE projection_state SET min_change_seq = ? WHERE id = 1 AND min_change_seq < ?`,
		keepFrom, keepFrom); err != nil {
		return 0, fmt.Errorf("readmodel: advance change floor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("readmodel: commit change prune: %w", err)
	}
	return removed, nil
}

// PublishDefinitionsChanged records a config reload in the change feed.
//
// Out-of-band relative to projection — no run moved — but it goes through the
// SAME ordered feed so a client learns about it at a definite cursor position
// rather than through a second channel with its own latency and failure modes.
//
// The row carries no run id or workflow scope: a definitions change can affect
// any of them, so a scoped invalidation would under-report. Clients treat it as
// an instance-and-workflow-wide refresh.
func (s *Store) PublishDefinitionsChanged(ctx context.Context) error {
	db, release, err := s.writeHandle()
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("readmodel: begin definitions change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := appendChange(ctx, tx, s.now(), ChangeDefinitionsChanged, RunRow{}); err != nil {
		return err
	}
	return tx.Commit()
}
