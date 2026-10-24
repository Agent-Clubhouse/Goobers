//go:build cloudintegration

package projectorpg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/cloud/projectorpg"
)

// interruptCommit cancels exactly when the selected transaction reaches COMMIT,
// after WithFence's pre-commit cancellation checks. Cancelling only the query
// instead of Serve models a failed commit unrelated to requested shutdown.
type interruptCommit struct {
	statement string
	shutdown  context.CancelFunc
	pending   sync.Map
	fired     atomic.Bool
	commitErr chan error
}

type interruptedCommitKey struct{}

func (c *interruptCommit) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, c.statement) {
		c.pending.Store(conn, true)
	}
	if !strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		return ctx
	}
	if _, ok := c.pending.LoadAndDelete(conn); !ok || !c.fired.CompareAndSwap(false, true) {
		return ctx
	}
	if c.shutdown != nil {
		c.shutdown()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	return context.WithValue(ctx, interruptedCommitKey{}, true)
}

func (c *interruptCommit) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(interruptedCommitKey{}) == true {
		c.commitErr <- data.Err
	}
}

func TestServeReportsInterruptedCommitsOnlyWithoutRequestedShutdown(t *testing.T) {
	for _, target := range []struct{ name, statement string }{
		{"projection", `INSERT INTO "readmodel_v1"."source_watermark"`},
		{"pass record", "SET last_pass_at = clock_timestamp()"},
	} {
		for _, shutdown := range []bool{true, false} {
			name := target.name + "/commit failure"
			if shutdown {
				name = target.name + "/requested shutdown"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.emit(t, runID('e'), true)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				tracer := &interruptCommit{statement: target.statement, commitErr: make(chan error, 1)}
				if shutdown {
					tracer.shutdown = cancel
				}
				config := f.controller.Config().Copy()
				config.ConnConfig.Tracer = tracer
				pool, err := pgxpool.NewWithConfig(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				manager, err := lease.New(lease.Config{Pool: pool, InstanceID: pgtest.InstanceID, Name: projectorpg.LeaseName, Holder: "interrupted-projector", TTL: 2 * time.Minute, SelfFenceAfter: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				var reported []error
				done := make(chan error, 1)
				go func() {
					done <- projectorpg.Serve(ctx, projectorpg.ServeConfig{
						Manager: manager, Pool: pool, Journal: f.backend, InstanceID: pgtest.InstanceID,
						Report: func(err error) {
							reported = append(reported, err)
							cancel()
						},
					})
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(30 * time.Second):
					cancel()
					<-done
					t.Fatal("Serve did not stop after the interrupted commit")
				}
				select {
				case err := <-tracer.commitErr:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("COMMIT returned %v, want cancellation", err)
					}
				default:
					t.Fatal("selected transaction never reached COMMIT")
				}
				if shutdown {
					if len(reported) != 0 {
						t.Fatalf("requested shutdown reported %v", reported)
					}
				} else if len(reported) != 1 || !errors.Is(reported[0], lease.ErrCommitUnknown) {
					t.Fatalf("reported %v, want the unknown commit outcome", reported)
				}
			})
		}
	}
}
