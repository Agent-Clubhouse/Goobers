package readmodel

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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
		if _, _, err := store.ProjectionFloor(ctx); !errors.Is(err, ErrClosed) {
			t.Errorf("error = %v, want ErrClosed", err)
		}
	})
	t.Run("State", func(t *testing.T) {
		if _, err := store.State(ctx); !errors.Is(err, ErrClosed) {
			t.Errorf("error = %v, want ErrClosed", err)
		}
	})
	t.Run("ListRuns", func(t *testing.T) {
		if _, err := store.ListRuns(ctx, ListOptions{Limit: 1}); !errors.Is(err, ErrClosed) {
			t.Errorf("error = %v, want ErrClosed", err)
		}
	})
	t.Run("UpsertRun", func(t *testing.T) {
		if err := store.UpsertRun(ctx, Projection{Run: RunRow{RunID: "x"}}); !errors.Is(err, ErrClosed) {
			t.Errorf("error = %v, want ErrClosed", err)
		}
	})
}

func TestCloseWaitsForInFlightOperation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	db, release, err := store.readHandle()
	if err != nil {
		t.Fatalf("lease read handle: %v", err)
	}
	rows, err := db.Query(`SELECT schema_version FROM projection_state`)
	if err != nil {
		release()
		t.Fatalf("query: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while the query lease was active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	_ = rows.Close()
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the query completed")
	}
}

func TestResolveReadHandleFallsBackToWriter(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "read.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	readerPool := openReaderPool("")
	if readerPool != nil {
		t.Fatal("empty path unexpectedly opened a reader pool")
	}
	reader := resolveReadHandle(store.writer, readerPool)
	if reader != store.writer {
		t.Fatal("nil reader pool did not resolve to the writer")
	}
	if err := reader.Ping(); err != nil {
		t.Fatalf("fallback reader: %v", err)
	}
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
