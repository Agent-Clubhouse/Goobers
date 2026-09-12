package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDaemonReadinessStoppedByShutdown(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{
			name: "root cancellation",
			ctx:  canceled,
			err:  fmt.Errorf("initialize active-run counts: %w", context.Canceled),
			want: true,
		},
		{
			name: "root shutdown with wrapped deadline",
			ctx:  deadline,
			err:  fmt.Errorf("initialize active-run counts: %w", context.DeadlineExceeded),
			want: true,
		},
		{
			name: "error does not match root shutdown cause",
			ctx:  canceled,
			err:  fmt.Errorf("initialize active-run counts: %w", context.DeadlineExceeded),
		},
		{
			name: "readiness private timeout while root remains live",
			ctx:  context.Background(),
			err:  fmt.Errorf("initialize active-run counts: %w", context.DeadlineExceeded),
		},
		{
			name: "unrelated startup failure during shutdown",
			ctx:  canceled,
			err:  errors.New("read model unavailable"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := daemonReadinessStoppedByShutdown(test.ctx, test.err); got != test.want {
				t.Fatalf("daemonReadinessStoppedByShutdown() = %t, want %t", got, test.want)
			}
		})
	}
}
