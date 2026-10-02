package readservice

import (
	"context"
	"log"
	"sync"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readmodel/projector"
	"github.com/goobers/goobers/internal/readmodel/repair"
)

// StartStandaloneProjection keeps a standalone dashboard's cached read model
// converging on the journals while it serves (#5120).
//
// EnsureBuilt only builds an EMPTY cache. Without this, a populated cache was
// never written again: a run started by a separate `goobers run` after the
// cache was built stayed missing from every list, and a run the cache recorded
// as running stayed running after its journal finished — across dashboard
// restarts too, because the cache outlives the process.
//
// The daemon's projector is driven by intake watermarks in the instance; the
// standalone dashboard must leave the instance untouched, so it runs the same
// projector with no intake at all and relies on the repair sweep instead. The
// sweep discovers unprojected runs and refreshes rows still claiming `running`
// after their journal reached a terminal event, at a fixed I/O budget. It reads
// journals the way EnsureBuilt and the standalone run-detail route already do
// (journal.OpenRead), so a current-schema journal is only read; a journal on an
// older schema is upgraded in place exactly as those paths would. A run resumed
// from a terminal phase is not re-read: with no intake there is no marker to
// say it changed. Its completed cycles are also what lets the freshness
// envelope stop reporting no_sweep_completed. The restart pass heals rows an
// earlier session left non-terminal before the first cycle reaches them.
func StartStandaloneProjection(store *readmodel.Store, l instance.Layout, errorLog *log.Logger) func() {
	ctx, cancel := context.WithCancel(context.Background())
	p := projector.New(store, standaloneIntake{}, projector.Options{
		ResolveRunsDirs: l.RunDirsContext, Feed: store.Feed(),
	})
	stopProjector := p.Start(ctx)
	sweeper := repair.New(store, p, nil, repair.Options{ResolveRunsDirs: l.RunDirsContext})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Restart(ctx); err != nil && ctx.Err() == nil {
			errorLog.Printf("dashboard read model restart pass: %v", err)
		}
		sweeper.Run(ctx)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			stopProjector()
		})
	}
}

// standaloneIntake is the empty watermark source for the standalone projector:
// intake.db belongs to the instance, which a standalone dashboard never writes,
// so nothing is ever pending and nothing is ever acknowledged.
type standaloneIntake struct{}

func (standaloneIntake) Pending(context.Context, int) ([]intake.Marker, error) { return nil, nil }
func (standaloneIntake) Ack(context.Context, string, uint64) (bool, error)     { return false, nil }
func (standaloneIntake) AckRemoval(context.Context, string) error              { return nil }
