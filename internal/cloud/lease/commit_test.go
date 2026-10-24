package lease

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestCommitOutcomeUnknownIsNeverASelfFence: once COMMIT is sent, only a
// server error proves a rollback. A lost reply is an unknown outcome even when
// the self-fence deadline is the reason it was lost, and it must not read as
// ErrSelfFenced or ErrFencedTransactionTimeout, which promise nothing
// committed.
func TestCommitOutcomeUnknownIsNeverASelfFence(t *testing.T) {
	fenced, cancel := context.WithCancelCause(context.Background())
	cancel(ErrSelfFenced)
	for name, tc := range map[string]struct {
		ctx     context.Context
		err     error
		unknown bool
	}{
		"server rejected":             {ctx: context.Background(), err: &pgconn.PgError{Severity: "ERROR", Code: "40001"}},
		"connection lost":             {ctx: context.Background(), err: io.ErrUnexpectedEOF, unknown: true},
		"self-fence while committing": {ctx: fenced, err: context.DeadlineExceeded, unknown: true},
		// The server ending the session proves nothing about COMMIT.
		"backend terminated":       {ctx: context.Background(), err: &pgconn.PgError{Severity: "FATAL", Code: "57P01"}, unknown: true},
		"server crash shutdown":    {ctx: context.Background(), err: &pgconn.PgError{Severity: "FATAL", Code: "57P02"}, unknown: true},
		"connection failure class": {ctx: context.Background(), err: &pgconn.PgError{Severity: "ERROR", Code: "08006"}, unknown: true},
		"panic":                    {ctx: context.Background(), err: &pgconn.PgError{Severity: "PANIC", Code: "XX000"}, unknown: true},
		// Under a non-English lc_messages only the unlocalized field says FATAL.
		"localized fatal": {ctx: context.Background(), err: &pgconn.PgError{Severity: "SCHWERWIEGEND", SeverityUnlocalized: "FATAL", Code: "XX000"}, unknown: true},
		"localized error": {ctx: context.Background(), err: &pgconn.PgError{Severity: "FEHLER", SeverityUnlocalized: "ERROR", Code: "40001"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := commitError(tc.ctx, tc.err)
			if errors.Is(err, ErrCommitUnknown) != tc.unknown {
				t.Fatalf("commitError = %v, unknown outcome %v", err, tc.unknown)
			}
			if errors.Is(err, ErrSelfFenced) || errors.Is(err, ErrFencedTransactionTimeout) {
				t.Fatalf("commitError = %v promises nothing committed", err)
			}
		})
	}
}
