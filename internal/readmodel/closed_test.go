package readmodel

import (
	"context"
	"path/filepath"
	"testing"
)

// TestReadAfterCloseErrorsRatherThanPanicking pins the distinction that broke
// the first version of this fix.
//
// That version nilled the handle fields on Close so a post-Close query would
// "fail cleanly". It does not: QueryContext on a NIL *sql.DB dereferences it and
// segfaults (database/sql.(*DB).conn). The race shard caught it as a panic in
// the repair sweep, which legitimately reads the store while the daemon is
// shutting down.
//
// A CLOSED *sql.DB is safe to call and returns "sql: database is closed". That
// is the difference between a read racing shutdown reporting an error and it
// taking the process down.
func TestReadAfterCloseErrorsRatherThanPanicking(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every one of these is reachable from a goroutine still draining at
	// shutdown: the repair sweep, the projector's commit loop, and an SSE
	// subscription's pump.
	t.Run("ProjectionFloor", func(t *testing.T) {
		if _, _, err := store.ProjectionFloor(ctx); err == nil {
			t.Error("a read after Close returned no error")
		}
	})
	t.Run("State", func(t *testing.T) {
		if _, err := store.State(ctx); err == nil {
			t.Error("a read after Close returned no error")
		}
	})
	t.Run("ListRuns", func(t *testing.T) {
		if _, err := store.ListRuns(ctx, ListOptions{Limit: 1}); err == nil {
			t.Error("a read after Close returned no error")
		}
	})
	t.Run("UpsertRun", func(t *testing.T) {
		if err := store.UpsertRun(ctx, Projection{Run: RunRow{RunID: "x"}}); err == nil {
			t.Error("a write after Close returned no error")
		}
	})
}

// TestCloseIsIdempotent pins that a double Close is safe.
//
// The daemon has several shutdown paths that can each reach it, and a panic
// during shutdown is as bad as one during serving.
func TestCloseIsIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("second close returned %v; Close must be idempotent", err)
	}
}
