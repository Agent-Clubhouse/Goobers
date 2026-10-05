package rollup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestQueryRowsIterationError(t *testing.T) {
	iterationErr := errors.New("iteration failed")
	scan := func(rows *sql.Rows) (int64, error) {
		var value int64
		return value, rows.Scan(&value)
	}

	for _, message := range []string{
		"rollup: iterate model usage",
		"rollup: iterate work items",
		"rollup: iterate work item actions",
	} {
		t.Run(message, func(t *testing.T) {
			db, rows := openIterationErrorDB(t, iterationErr)

			got, err := queryRows(context.Background(), db, "SELECT value", nil, "query rows", scan, message)
			if got != nil {
				t.Fatalf("rows = %v, want nil", got)
			}
			if !errors.Is(err, iterationErr) {
				t.Fatalf("error = %v, want wrapped iteration error", err)
			}
			if want := message + ": " + iterationErr.Error(); err.Error() != want {
				t.Fatalf("error = %q, want %q", err, want)
			}
			if !rows.closed {
				t.Fatal("rows were not closed")
			}
		})
	}

	t.Run("unwrapped preserves partial rows", func(t *testing.T) {
		db, rows := openIterationErrorDB(t, iterationErr)

		got, err := queryRows(context.Background(), db, "SELECT value", nil, "query rows", scan)
		if !errors.Is(err, iterationErr) {
			t.Fatalf("error = %v, want iteration error", err)
		}
		if errors.Unwrap(err) != nil {
			t.Fatalf("error = %v, want unwrapped error", err)
		}
		if len(got) != 1 || got[0] != 7 {
			t.Fatalf("rows = %v, want [7]", got)
		}
		if !rows.closed {
			t.Fatal("rows were not closed")
		}
	})
}

type iterationErrorDriver struct {
	err  error
	rows *iterationErrorRows
}

func (d *iterationErrorDriver) Open(string) (driver.Conn, error) {
	d.rows = &iterationErrorRows{err: d.err}
	return &iterationErrorConn{rows: d.rows}, nil
}

type iterationErrorConn struct {
	rows *iterationErrorRows
}

func (c *iterationErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *iterationErrorConn) Close() error {
	return nil
}

func (c *iterationErrorConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *iterationErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return c.rows, nil
}

type iterationErrorRows struct {
	err    error
	read   bool
	closed bool
}

func (*iterationErrorRows) Columns() []string {
	return []string{"value"}
}

func (r *iterationErrorRows) Close() error {
	r.closed = true
	return nil
}

func (r *iterationErrorRows) Next(dest []driver.Value) error {
	if !r.read {
		r.read = true
		dest[0] = int64(7)
		return nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return err
	}
	return io.EOF
}

func openIterationErrorDB(t *testing.T, iterationErr error) (*sql.DB, *iterationErrorRows) {
	t.Helper()

	name := fmt.Sprintf("rollup-iteration-error-%s", t.Name())
	d := &iterationErrorDriver{err: iterationErr}
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	return db, d.rows
}
