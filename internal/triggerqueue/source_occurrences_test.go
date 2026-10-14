package triggerqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOccurrenceCursorAtomicityRestartAndRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s := openTestStore(t, path)
	base := time.Now().UTC().Truncate(time.Minute)
	if _, _, err := s.SourceCursorRevision(t.Context(), "scope", "A", base, base, false); err != nil {
		t.Fatal(err)
	}
	if positions, _, err := s.SourceOccurrences(t.Context(), "scope", "A", 2); err != nil || len(positions) != 2 {
		t.Fatal(positions, err)
	}
	batch := SourceBatch{Key: "one", Actor: "scheduler", Fingerprint: "one", Starts: []SourceStart{{Payload: []byte("pins")}}, Advance: &SourceAdvance{Scope: "scope", Revision: "A", Occurrence: 1, Before: base, After: base.Add(time.Minute)}}
	if _, _, err := s.AcceptSource(t.Context(), batch, base); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	positions, _, err := s.SourceOccurrences(t.Context(), "scope", "A", 2)
	if err != nil || !positions[0].Equal(base.Add(time.Minute)) || !positions[1].Equal(base) {
		t.Fatal(positions, err)
	}
	batch.Key = "stale"
	batch.Fingerprint = "stale"
	if _, _, err = s.AcceptSource(t.Context(), batch, base); !errors.Is(err, ErrTransition) {
		t.Fatal("stale position", err)
	}
	batch.Advance.Occurrence = 0
	if _, _, err = s.AcceptSource(t.Context(), batch, base); !errors.Is(err, ErrTransition) {
		t.Fatal("coalesced writer replaced independent positions", err)
	}
	if records, err := s.Pending(t.Context(), 10); err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	if _, _, err = s.SourceCursorRevision(t.Context(), "scope", "B", base, base.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	positions, _, err = s.SourceOccurrences(t.Context(), "scope", "B", 3)
	if err != nil || len(positions) != 3 || !positions[2].Equal(base.Add(time.Hour)) {
		t.Fatal(positions, err)
	}
	batch.Advance.Occurrence = 2
	if _, _, err = s.AcceptSource(t.Context(), batch, base); !errors.Is(err, ErrTransition) {
		t.Fatal("old revision", err)
	}
}

func TestOccurrenceCursorCapacityRollbackAndLegacyAdoption(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	base := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.SourceCursorRevision(t.Context(), "scope", "A", base, base, true); err != nil {
		t.Fatal(err)
	}
	if positions, pending, err := s.SourceOccurrences(t.Context(), "scope", "A", 2); err != nil || !pending || positions != nil {
		t.Fatal(positions, pending, err)
	}
	batch := SourceBatch{Key: "legacy", Actor: "scheduler", Fingerprint: "legacy", Advance: &SourceAdvance{Scope: "scope", Revision: "A", Before: base, After: base.Add(time.Minute)}}
	if _, _, err := s.AcceptSource(t.Context(), batch, base); err != nil {
		t.Fatal(err)
	}
	if positions, pending, err := s.SourceOccurrences(t.Context(), "scope", "A", 2); err != nil || pending || !positions[0].Equal(base.Add(time.Minute)) {
		t.Fatal(positions, pending, err)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<9997) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'old-'||x,'old-'||x,'scheduler',x,'accepted',1 FROM n`); err != nil {
		t.Fatal(err)
	}
	batch.Key = "full"
	batch.Fingerprint = "full"
	batch.Starts = []SourceStart{{Payload: []byte("new")}}
	batch.Advance.Occurrence = 2
	batch.Advance.Before = base.Add(time.Minute)
	batch.Advance.After = base.Add(2 * time.Minute)
	if _, _, err := s.AcceptSource(t.Context(), batch, base); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	positions, _, err := s.SourceOccurrences(t.Context(), "scope", "A", 2)
	if err != nil || !positions[1].Equal(base.Add(time.Minute)) {
		t.Fatal(positions, err)
	}
}

// Previous releases hashed this exact structure. An absent occurrence index must
// not alter retained coalesced/independent capture retries after the migration.
func TestOccurrenceCursorPreservesPriorDemandFingerprint(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	capture := ScheduleDemandCapture{ID: "old", Advance: SourceAdvance{Scope: "scope", Revision: "revision", Before: base, After: base.Add(time.Hour)}, Payload: []byte("pins"), Independent: true, AcceptedAt: base.Add(time.Hour)}
	legacy := struct {
		ID      string
		Advance struct {
			Scope         string
			Revision      string
			Before, After time.Time
		}
		Payload     []byte
		Independent bool
		AcceptedAt  time.Time
	}{ID: capture.ID, Payload: capture.Payload, Independent: true}
	legacy.Advance.Scope = capture.Advance.Scope
	legacy.Advance.Revision = capture.Advance.Revision
	legacy.Advance.Before = capture.Advance.Before
	legacy.Advance.After = capture.Advance.After
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	got, err := scheduleDemandFingerprint(capture)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatal("existing replay identity changed", got, err)
	}
	capture.Advance.Occurrence = 1
	changed, err := scheduleDemandFingerprint(capture)
	if err != nil || changed == got {
		t.Fatal("declaration did not distinguish identity", changed, err)
	}
}
