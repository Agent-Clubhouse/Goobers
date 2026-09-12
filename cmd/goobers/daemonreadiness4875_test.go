package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestDaemonStartupStoppedByShutdown(t *testing.T) {
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
			if got := daemonStartupStoppedByShutdown(test.ctx, test.err); got != test.want {
				t.Fatalf("daemonStartupStoppedByShutdown() = %t, want %t", got, test.want)
			}
		})
	}
}

// This pins cancellation classification before the active-count readiness
// gate. The projection constructor is a synchronous, context-derived startup
// operation; a shutdown that reaches it must have the same clean exit contract
// as a shutdown while readiness is sampling the read model.
func TestDaemonStartupStopsCleanlyWhenEngineProjectionIsCanceled(t *testing.T) {
	root := initDeterministicDemo(t)
	ctx, cancel := context.WithCancel(context.Background())

	original := startEngineProjection
	startEngineProjection = func(daemonCtx context.Context, _ instance.Layout, _ *instance.Config, _ *instance.ConfigSet,
		_ *daemonEngineClient, _ *intake.Store, _ *journal.InstanceLog, _ *telemetry.Client,
		_ *livejournal.Writer, _ blobstore.Store) (func(), error) {
		cancel()
		<-daemonCtx.Done()
		return nil, daemonCtx.Err()
	}
	t.Cleanup(func() { startEngineProjection = original })

	var stdout, stderr bytes.Buffer
	if code := runUpContext(ctx, []string{"--quiet", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("pre-readiness projection shutdown code=%d, want documented clean-shutdown code 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "error: start engine projection reconciler:") {
		t.Fatalf("clean pre-readiness projection shutdown reported a startup failure: %s", stderr.String())
	}
}
