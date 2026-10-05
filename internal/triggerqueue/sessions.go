package triggerqueue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/sessioning"
)

// ErrSessionClosed and ErrSessionExpired distinguish closed intake from an
// expired idempotency receipt. Expiry never authorizes replay of old effects.
var (
	ErrSessionClosed  = errors.New("session intake is closed")
	ErrSessionExpired = errors.New("session request has expired")
)

const sessionResponseAllowance = sessioning.MaxTextBytes + 16*1024
const sessionColumns = "id,gaggle,title,profile,state,creator,created_ns,updated_ns,next_sequence,active_turn,last_outcome"
const sessionMessageColumns = "id,session_id,sequence,actor_kind,actor,text,created_ns,turn_id,run_id,outcome,repair_target"

// SessionCommand is trusted host admission metadata; callers authorize Actor
// against the current gaggle policy and scrub all content before this boundary.
type SessionCommand struct {
	Gaggle        string
	Actor         sessioning.Actor
	RequestID     string
	RequestDigest string
}

func (c SessionCommand) valid() bool {
	return validChildText(c.Gaggle, 128, true) && validChildText(c.Actor.Issuer, 2048, true) &&
		!strings.HasPrefix(c.Actor.Issuer, "goobers/") && validChildText(c.Actor.Subject, 512, true) &&
		validChildText(c.RequestID, 256, true) && validSessionDigest(c.RequestDigest)
}

func validSessionDigest(v string) bool {
	if len(v) != 71 || !strings.HasPrefix(v, "sha256:") {
		return false
	}
	return strings.Trim(v[7:], "0123456789abcdef") == ""
}
func sessionKey(c SessionCommand, kind, scope string) string {
	raw, _ := json.Marshal([]string{c.Gaggle, c.Actor.Issuer, c.Actor.Subject, kind, scope, c.RequestID})
	return "sha256:" + childDigest(raw)
}

func scanSession(row scanner) (sessioning.Session, error) {
	var s sessioning.Session
	var profile, actor []byte
	var created, updated int64
	err := row.Scan(&s.ID, &s.Gaggle, &s.Title, &profile, &s.State, &actor, &created, &updated, &s.NextSequence, &s.ActiveTurnID, &s.LastOutcome)
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(profile, &s.Profile); err != nil {
		return s, err
	}
	if err = json.Unmarshal(actor, &s.CreatedBy); err != nil {
		return s, err
	}
	s.CreatedAt = time.Unix(0, created).UTC()
	s.UpdatedAt = time.Unix(0, updated).UTC()
	return s, nil
}
func scanSessionMessage(row scanner) (sessioning.Message, error) {
	var m sessioning.Message
	var actor, target []byte
	var created int64
	err := row.Scan(&m.ID, &m.SessionID, &m.Sequence, &m.ActorKind, &actor, &m.Text, &created, &m.TurnID, &m.RunID, &m.Outcome, &target)
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(actor, &m.Actor); err != nil {
		return m, err
	}
	m.RepairTarget, err = sessioning.ParsePRRepairTarget(target)
	if err != nil || (m.RepairTarget != nil && m.ActorKind != "human") {
		return m, ErrTransition
	}
	m.CreatedAt = time.Unix(0, created).UTC()
	return m, nil
}

func sessionAcceptance(ctx context.Context, tx *sql.Tx, gaggle, id, message, acceptance string) (sessioning.Acceptance, error) {
	s, err := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM interactive_sessions WHERE gaggle=? AND id=?", gaggle, id))
	result := sessioning.Acceptance{Session: s, AcceptanceID: acceptance}
	if err != nil || message == "" {
		return result, err
	}
	m, err := scanSessionMessage(tx.QueryRowContext(ctx, "SELECT "+sessionMessageColumns+" FROM interactive_messages WHERE session_id=? AND id=?", id, message))
	result.Message = &m
	return result, err
}

func sessionReplay(ctx context.Context, tx *sql.Tx, c SessionCommand, kind, scope string) (sessioning.Acceptance, bool, error) {
	var digest, id, message, acceptance string
	var expired sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT request_digest,session_id,message_id,acceptance_id,tombstoned_ns FROM interactive_requests WHERE key_digest=?`, sessionKey(c, kind, scope)).Scan(&digest, &id, &message, &acceptance, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return sessioning.Acceptance{}, false, nil
	}
	if err != nil {
		return sessioning.Acceptance{}, false, err
	}
	if digest != c.RequestDigest {
		return sessioning.Acceptance{}, false, ErrConflict
	}
	if expired.Valid {
		return sessioning.Acceptance{}, false, ErrSessionExpired
	}
	result, err := sessionAcceptance(ctx, tx, c.Gaggle, id, message, acceptance)
	result.Duplicate = true
	return result, true, err
}

// SessionReplay checks an exact command before expensive profile preparation.
// It does not authorize access; the current human policy must still allow it.
func (s *Store) SessionReplay(ctx context.Context, c SessionCommand, kind, scope string) (sessioning.Acceptance, bool, error) {
	if !c.valid() || (kind != "create" && kind != "message" && kind != "close") {
		return sessioning.Acceptance{}, false, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessioning.Acceptance{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	return sessionReplay(ctx, tx, c, kind, scope)
}

func keepSessionRequest(ctx context.Context, tx *sql.Tx, c SessionCommand, kind, scope, id, message, acceptance string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO interactive_requests(key_digest,request_digest,gaggle,session_id,message_id,acceptance_id,created_ns) VALUES(?,?,?,?,?,?,?)`, sessionKey(c, kind, scope), c.RequestDigest, c.Gaggle, id, message, acceptance, now.UnixNano())
	return err
}

func sessionCapacity(ctx context.Context, tx *sql.Tx, gaggle string, additional int) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM interactive_sessions WHERE gaggle=?)+(SELECT COUNT(*) FROM interactive_requests WHERE gaggle=?)`, gaggle, gaggle).Scan(&count)
	if err != nil {
		return err
	}
	if count+additional > sessioning.MaxRetainedRecords {
		return ErrFull
	}
	return nil
}

// CreateSession persists a pinned profile without reserving an execution slot.
func (s *Store) CreateSession(ctx context.Context, c SessionCommand, title string, profile sessioning.Profile, now time.Time) (sessioning.Acceptance, error) {
	if !c.valid() || !validChildText(title, sessioning.MaxTitleBytes, true) || !validChildText(profile.Goober, 128, true) || !validSessionDigest(profile.ConfigGeneration) || !validSessionDigest(profile.GooberDigest) || now.IsZero() {
		return sessioning.Acceptance{}, ErrTransition
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if result, found, err := sessionReplay(ctx, tx, c, "create", ""); err != nil || found {
		return result, err
	}
	if err = sessionCapacity(ctx, tx, c.Gaggle, 2); err != nil {
		return sessioning.Acceptance{}, err
	}
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM interactive_sessions WHERE gaggle=? AND state!='closed'`, c.Gaggle).Scan(&open); err != nil {
		return sessioning.Acceptance{}, err
	}
	if open >= sessioning.MaxOpenSessions {
		return sessioning.Acceptance{}, ErrFull
	}
	if err = childByteCapacity(ctx, tx, 8192); err != nil {
		return sessioning.Acceptance{}, err
	}
	id := fmt.Sprintf("session-%x", randomID())
	p, _ := json.Marshal(profile)
	actor, _ := json.Marshal(c.Actor)
	_, err = tx.ExecContext(ctx, `INSERT INTO interactive_sessions(id,gaggle,title,profile,creator,state,created_ns,updated_ns) VALUES(?,?,?,?,?,'idle',?,?)`, id, c.Gaggle, title, p, actor, now.UnixNano(), now.UnixNano())
	if err != nil {
		return sessioning.Acceptance{}, err
	}
	if err = keepSessionRequest(ctx, tx, c, "create", "", id, "", "", now); err != nil {
		return sessioning.Acceptance{}, err
	}
	result, err := sessionAcceptance(ctx, tx, c.Gaggle, id, "", "")
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func validSessionText(text string) bool {
	return text != "" && len(text) <= sessioning.MaxTextBytes && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}
