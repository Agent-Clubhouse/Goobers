package triggerqueue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func publicationRetentionIntent(t *testing.T, s *Store, id string, now time.Time) EventPublication {
	t.Helper()
	req := eventRequest(id, false)
	req.Producer.RunID = "producer-run"
	req.Producer.RootID = "producer-run"
	req.Producer.Stage = "publish"
	p, _, err := s.BeginEventPublication(t.Context(), EventPublication{ID: id, ConfigGeneration: "generation", Occurrence: "visit-" + id, Acceptance: req}, now)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublicationTerminalFenceAndBoundedThroughput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	now := time.Now().UTC()
	var first EventPublication
	proof := EventProducerTerminal{Gaggle: "web", RunID: "producer-run", ConfigGeneration: "generation", Phase: "completed", Sequence: 900}
	for i := range 253 {
		p := publicationRetentionIntent(t, s, fmt.Sprintf("publication-%04d", i), now)
		if i == 0 {
			first = p
		}
		if err := s.SettleEventPublication(t.Context(), p.ID, proof, now); err != nil {
			t.Fatal(err)
		}
	}
	resumable := publicationRetentionIntent(t, s, "resumable", now)
	for _, phase := range []string{"failed", "escalated", "interrupted", "running"} {
		bad := proof
		bad.Phase = phase
		if err := s.SettleEventPublication(t.Context(), resumable.ID, bad, now); !errors.Is(err, ErrTransition) {
			t.Fatal(phase, err)
		}
	}
	fillPublicationCapacity(t, s, MaxEventPublications-254, now)
	overflow := resumable
	overflow.ID = "overflow"
	overflow.Acceptance = eventRequest("overflow", false)
	overflow.Acceptance.Producer = resumable.Acceptance.Producer
	if _, _, err := s.BeginEventPublication(t.Context(), overflow, now); !errors.Is(err, ErrFull) {
		t.Fatalf("full active capacity: %v", err)
	}
	wrong := proof
	wrong.ConfigGeneration = "wrong"
	if err := s.SettleEventPublication(t.Context(), first.ID, wrong, now); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if n, err := s.PruneEventPublications(t.Context(), now.Add(EventRetention-time.Nanosecond), 100); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	for _, want := range []int{100, 100, 53, 0} {
		n, err := s.PruneEventPublications(t.Context(), now.Add(EventRetention), 100)
		if err != nil || n != want {
			t.Fatal(n, want, err)
		}
	}
	var live, tombs int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM event_outbox").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM event_publication_tombstones").Scan(&tombs); err != nil {
		t.Fatal(err)
	}
	if live != MaxEventPublications-253 || tombs != 253 {
		t.Fatalf("live capacity=%d tombstones=%d", live, tombs)
	}
	// Multiple batches freed 253 active slots without evicting resumable custody.
	for i := range 253 {
		publicationRetentionIntent(t, s, fmt.Sprintf("next-%04d", i), now.Add(EventRetention))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	observed, err := s.ObserveEventPublication(t.Context(), first)
	if err != nil || observed.TerminalSequence != 900 || observed.ReceiptID != "" {
		t.Fatal(observed, err)
	}
	if _, _, err = s.BeginEventPublication(t.Context(), first, now.Add(EventRetention)); !errors.Is(err, ErrEventPublicationSettled) {
		t.Fatalf("late replay=%v", err)
	}
	changed := first
	changed.Acceptance.Envelope = []byte(strings.Replace(string(first.Acceptance.Envelope), "42", "43", 1))
	if _, err = s.ObserveEventPublication(t.Context(), changed); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for _, want := range []int{50, 50, 50, 50, 50, 3, 0} {
		n, err := s.PruneEventPublications(t.Context(), now.Add(EventRetention+EventTombstoneRetention), 100)
		if err != nil || n != want {
			t.Fatal(n, want, err)
		}
	}
	if _, err = s.ObserveEventPublication(t.Context(), first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}

func TestPublicationTerminalFenceRecoversLostReceiptAcknowledgement(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	now := time.Now().UTC()
	p := publicationRetentionIntent(t, s, "accepted-no-reply", now)
	receipt, _, err := s.AcceptEvent(t.Context(), p.Acceptance, now)
	if err != nil {
		t.Fatal(err)
	}
	proof := EventProducerTerminal{Gaggle: "web", RunID: "producer-run", ConfigGeneration: "generation", Phase: "aborted", Sequence: 44}
	if err = s.SettleEventPublication(t.Context(), p.ID, proof, now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PruneEventPublications(t.Context(), now.Add(EventRetention), 100); err != nil {
		t.Fatal(err)
	}
	observed, err := s.ObserveEventPublication(t.Context(), p)
	if err != nil || observed.ReceiptID != receipt.ID {
		t.Fatal(observed, err)
	}
	if _, _, err = s.BeginEventPublication(t.Context(), p, now.Add(EventRetention)); !errors.Is(err, ErrEventPublicationSettled) {
		t.Fatal(err)
	}
}

// Build a valid large fixture in one transaction; public admission is exercised
// at the full boundary and for every newly freed slot after compaction.
func fillPublicationCapacity(t *testing.T, s *Store, count int, now time.Time) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range count {
		id := fmt.Sprintf("pending-%04d", i)
		req := eventRequest(id, false)
		req.Producer.RunID = "producer-run"
		req.Producer.RootID = "producer-run"
		req.Producer.Stage = "publish"
		envelope, _, _, err := validateEventAcceptance(req, now)
		if err != nil {
			t.Fatal(err)
		}
		req.Envelope = envelope.JSON
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(`INSERT INTO event_outbox(id,gaggle,run_id,generation,occurrence,envelope_digest,request,created_ns) VALUES(?,?,?,?,?,?,?,?)`, id, "web", "producer-run", "generation", "visit-"+id, envelope.Digest, raw, now.UnixNano())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRetentionMigrationPreservesSourceGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var prior []string
	for _, migration := range migrations {
		if migration == eventPublicationRetentionSchema {
			break
		}
		prior = append(prior, migration)
	}
	if len(prior) == len(migrations) {
		t.Fatal("retention migration missing")
	}
	if err = sqliteschema.Migrate(t.Context(), db, "triggerqueue", prior); err != nil {
		t.Fatal(err)
	}
	req := eventRequest("prior", false)
	req.Producer.RunID = "producer-run"
	req.Producer.Stage = "publish"
	req.Producer.RootGroupID = "source-group"
	req.Producer.CausationID = "source-group"
	req.Producer.RootSetDigest = "sha256:" + strings.Repeat("a", 64)
	req.Producer.Depth = 1
	envelope, _, _, err := validateEventAcceptance(req, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	req.Envelope = envelope.JSON
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO event_outbox(id,gaggle,run_id,generation,occurrence,envelope_digest,request,created_ns) VALUES(?,?,?,?,?,?,?,?)`, "prior", "web", "producer-run", "generation", "visit", envelope.Digest, raw, childTestTime.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	page, err := store.EventPublicationPage(t.Context(), "", 100)
	if err != nil || len(page) != 1 || page[0].Acceptance.Producer.RootGroupID != "source-group" || !page[0].SettledAt.IsZero() {
		t.Fatal(page, err)
	}
	var group string
	if err = store.db.QueryRow("SELECT source_group FROM event_outbox WHERE id='prior'").Scan(&group); err != nil || group != "source-group" {
		t.Fatal(group, err)
	}
}
