package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sqliteschema"
)

func TestSourceRevisionFencesStaleWriterAndPreservesAcceptedStarts(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	base := time.Now().UTC().Truncate(time.Second)
	due := base.Add(time.Minute)
	if _, _, err := s.SourceCursorRevision(t.Context(), "scope", "A", base, base, false); err != nil {
		t.Fatal(err)
	}
	batch := SourceBatch{Key: "first", Actor: "scheduler", Fingerprint: "first", Starts: []SourceStart{{Payload: []byte("original")}}, Advance: &SourceAdvance{Scope: "scope", Revision: "A", Before: base, After: due}}
	first, _, err := s.AcceptSource(t.Context(), batch, due)
	if err != nil {
		t.Fatal(err)
	}
	cursor, legacy, err := s.SourceCursorRevision(t.Context(), "scope", "B", base, due, true)
	if err != nil || legacy || !cursor.Equal(due) {
		t.Fatal(cursor, legacy, err)
	}
	batch.Key, batch.Fingerprint = "stale", "stale"
	batch.Advance.Before, batch.Advance.After = due, due.Add(time.Minute)
	if _, _, err = s.AcceptSource(t.Context(), batch, due); !errors.Is(err, ErrTransition) {
		t.Fatal("stale revision accepted", err)
	}
	pending, err := s.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != first.AcceptanceIDs[0] {
		t.Fatal(pending, err)
	}
	batch.Key, batch.Fingerprint, batch.Advance.Revision = "current", "current", "B"
	if _, _, err = s.AcceptSource(t.Context(), batch, due); err != nil {
		t.Fatal(err)
	}
	// Same revision ignores later initialization hints; unrelated edits cannot reset it.
	cursor, _, err = s.SourceCursorRevision(t.Context(), "scope", "B", due.Add(time.Hour), due.Add(time.Hour), true)
	if err != nil || !cursor.Equal(due.Add(time.Minute)) {
		t.Fatal(cursor, err)
	}
}

func TestSourceRevisionMigrationAdoptsOldPendingCursorOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(migrations, sourceRevisionSchema)
	if index < 1 {
		t.Fatal("missing additive revision migration")
	}
	if err := sqliteschema.Migrate(t.Context(), db, "triggerqueue", migrations[:index]); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := db.Exec(`INSERT INTO source_start_cursors(scope,cursor_ns,legacy_pending) VALUES('scope',?,1)`, base.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, path)
	cursor, pending, err := s.SourceCursorRevision(t.Context(), "scope", "A", base.Add(time.Hour), base.Add(time.Hour), false)
	if err != nil || !pending || !cursor.Equal(base) {
		t.Fatal(cursor, pending, err)
	}
	due := base.Add(time.Hour)
	batch := SourceBatch{Key: "legacy", Actor: "scheduler", Fingerprint: "legacy", Advance: &SourceAdvance{Scope: "scope", Revision: "A", Before: base, After: due}}
	if _, _, err := s.AcceptSource(t.Context(), batch, due); err != nil {
		t.Fatal(err)
	}
	cursor, pending, err = s.SourceCursorRevision(t.Context(), "scope", "A", base, base, true)
	if err != nil || pending || !cursor.Equal(due) {
		t.Fatal(cursor, pending, err)
	}
	cursor, pending, err = s.SourceCursorRevision(t.Context(), "scope", "B", base, due.Add(time.Minute), true)
	if err != nil || pending || !cursor.Equal(due.Add(time.Minute)) {
		t.Fatal(cursor, pending, err)
	}
}
