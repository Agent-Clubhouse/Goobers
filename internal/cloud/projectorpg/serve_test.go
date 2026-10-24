package projectorpg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/lease"
)

func TestShutdownReporterPreservesFailuresUnrelatedToShutdown(t *testing.T) {
	unknownCancellation := fmt.Errorf("record projector pass: %w: %w", lease.ErrCommitUnknown, context.Canceled)
	unknownConnection := fmt.Errorf("project run: %w: %w", lease.ErrCommitUnknown, io.ErrUnexpectedEOF)
	unknownDeadline := fmt.Errorf("record projector pass: %w: %w", lease.ErrCommitUnknown, context.DeadlineExceeded)
	for _, tt := range []struct {
		name     string
		shutdown bool
		deadline bool
		err      error
		want     bool
	}{
		{name: "requested shutdown", shutdown: true, err: unknownCancellation},
		{name: "requested deadline", deadline: true, err: unknownDeadline},
		{name: "cancelled before commit", shutdown: true, err: fmt.Errorf("read journal: %w", context.Canceled)},
		{name: "epoch cancellation", err: unknownCancellation, want: true},
		{name: "commit deadline", err: unknownDeadline, want: true},
		{name: "connection failure", err: unknownConnection, want: true},
		{name: "connection failure during shutdown", shutdown: true, err: unknownConnection, want: true},
		{name: "commit deadline during shutdown", shutdown: true, err: unknownDeadline, want: true},
		{name: "real failure during shutdown", shutdown: true, err: errors.New("invalid projection"), want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.shutdown {
				cancel()
			}
			if tt.deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(t.Context(), time.Time{})
				defer stop()
			}
			var reported []error
			shutdownReporter(ctx, func(err error) { reported = append(reported, err) })(tt.err)
			if tt.want {
				if len(reported) != 1 || reported[0] != tt.err {
					t.Fatalf("reported %v, want original error %v", reported, tt.err)
				}
			} else if len(reported) != 0 {
				t.Fatalf("shutdown reported %v", reported)
			}
		})
	}
}
