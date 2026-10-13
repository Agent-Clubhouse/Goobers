package sqlitebusy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestIsDetectsRealBusyError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	open := func() *sql.Conn {
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(0)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	holder, other := open(), open()
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	_, err := other.ExecContext(ctx, "BEGIN IMMEDIATE")
	if err == nil {
		t.Fatal("expected a busy error")
	}
	if !Is(fmt.Errorf("wrapped: %w", err)) {
		t.Errorf("Is(%v)=false, want true", err)
	}
}

func TestIsRejectsOtherErrors(t *testing.T) {
	if Is(nil) || Is(errors.New("boom")) {
		t.Error("non-sqlite errors must not be busy")
	}
}
