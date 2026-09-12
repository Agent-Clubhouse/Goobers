package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/readservice"
)

type statusProjection struct {
	readModel *readmodel.ExistingReader
	intake    *intake.ExistingReader
}

// openStatusProjection is deliberately fail-open to journals. Projection
// files are an optimisation: missing, stale, corrupt, or unreadable state must
// never make the authoritative status path unavailable.
func openStatusProjection(ctx context.Context, layout instance.Layout) statusProjection {
	if _, err := os.Stat(layout.ReadDB()); err != nil {
		return statusProjection{}
	}
	openContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	reader, err := readmodel.OpenExistingReader(openContext, layout.ReadDB())
	cancel()
	if err != nil {
		return statusProjection{}
	}
	stateContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	state, err := reader.State(stateContext)
	cancel()
	if err != nil || !state.Ready {
		_ = reader.Close()
		return statusProjection{}
	}
	if _, err := os.Stat(layout.IntakeDB()); err != nil {
		_ = reader.Close()
		return statusProjection{}
	}
	intakeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	intakeReader, err := intake.OpenExistingReader(intakeContext, layout.IntakeDB())
	cancel()
	if err != nil {
		_ = reader.Close()
		return statusProjection{}
	}
	return statusProjection{readModel: reader, intake: intakeReader}
}

func (p statusProjection) Close() error {
	var err error
	if p.intake != nil {
		err = errors.Join(err, p.intake.Close())
	}
	if p.readModel != nil {
		err = errors.Join(err, p.readModel.Close())
	}
	return err
}

type statusProjectedCursor struct {
	schema int
	epoch  string
	seq    uint64
	valid  bool
}

type statusProjectedFrame struct {
	display []runSummary
	fleet   []runSummary
	changed map[string]struct{}
	cursor  statusProjectedCursor
}

type statusRunLoader struct {
	layout    instance.Layout
	sources   readservice.LocalSources
	journal   *readservice.Local
	options   statusOptions
	needFleet bool

	projected bool
	fleetRuns []runSummary
	changed   map[string]struct{}
	cursor    statusProjectedCursor

	// afterProjectedQueries is a deterministic race seam for frame-fence tests.
	afterProjectedQueries func()
}

func (l *statusRunLoader) Load() ([]runSummary, error) {
	ctx := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		frame, ok := l.loadProjected(ctx)
		if ok {
			l.projected = true
			l.fleetRuns = frame.fleet
			l.changed = frame.changed
			l.cursor = frame.cursor
			return frame.display, nil
		}
	}
	runs, err := listStatusRuns(ctx, l.journal, l.options)
	if err != nil {
		return nil, err
	}
	l.projected = false
	l.fleetRuns = runs
	l.changed = nil
	l.cursor = statusProjectedCursor{}
	return runs, nil
}

func (l *statusRunLoader) loadProjected(ctx context.Context) (statusProjectedFrame, bool) {
	projection := openStatusProjection(ctx, l.layout)
	if projection.readModel == nil || projection.intake == nil {
		return statusProjectedFrame{}, false
	}
	defer func() { _ = projection.Close() }()

	before, err := projection.intake.Fence(ctx)
	if err != nil || before.Pending != 0 {
		return statusProjectedFrame{}, false
	}
	state, err := projection.readModel.State(ctx)
	if err != nil || !state.Ready {
		return statusProjectedFrame{}, false
	}
	cut, err := projection.readModel.LatestChangeSeq(ctx)
	if err != nil {
		return statusProjectedFrame{}, false
	}
	projectedSources := l.sources
	projectedSources.ReadModel = projection.readModel
	reads, err := readservice.NewLocal(projectedSources, func() bool { return true })
	if err != nil {
		return statusProjectedFrame{}, false
	}
	display, err := listStatusRuns(ctx, reads, l.options)
	if err != nil {
		return statusProjectedFrame{}, false
	}
	fleet := display
	if l.needFleet {
		facts, err := reads.StatusFleetFacts(ctx)
		if err != nil {
			return statusProjectedFrame{}, false
		}
		fleet = statusFleetRuns(facts)
	}
	if l.afterProjectedQueries != nil {
		l.afterProjectedQueries()
	}
	changed, cursor, err := statusChangesThrough(ctx, projection.readModel, l.cursor, state, cut)
	if err != nil {
		return statusProjectedFrame{}, false
	}
	afterState, err := projection.readModel.State(ctx)
	if err != nil {
		return statusProjectedFrame{}, false
	}
	afterCut, err := projection.readModel.LatestChangeSeq(ctx)
	if err != nil {
		return statusProjectedFrame{}, false
	}
	after, err := projection.intake.Fence(ctx)
	if err != nil || !sameStatusProjectionState(state, afterState) || afterCut != cut ||
		after.Pending != 0 || after.DataVersion != before.DataVersion {
		return statusProjectedFrame{}, false
	}
	return statusProjectedFrame{display: display, fleet: fleet, changed: changed, cursor: cursor}, true
}

func sameStatusProjectionState(a, b readmodel.State) bool {
	return a.Ready && b.Ready && a.SchemaVersion == b.SchemaVersion && a.Epoch == b.Epoch &&
		a.MinChangeSeq == b.MinChangeSeq
}

func statusChangesThrough(
	ctx context.Context,
	reader readmodel.Reader,
	previous statusProjectedCursor,
	state readmodel.State,
	cut uint64,
) (map[string]struct{}, statusProjectedCursor, error) {
	next := statusProjectedCursor{schema: state.SchemaVersion, epoch: state.Epoch, seq: cut, valid: true}
	if !previous.valid || previous.schema != state.SchemaVersion || previous.epoch != state.Epoch ||
		previous.seq < state.MinChangeSeq {
		return nil, next, nil
	}
	changed := make(map[string]struct{})
	seq := previous.seq
	for seq < cut {
		changes, err := reader.Changes(ctx, seq, 1000)
		if err != nil {
			return nil, statusProjectedCursor{}, err
		}
		if len(changes) == 0 {
			break
		}
		advanced := false
		for _, change := range changes {
			if change.Seq > cut {
				return changed, next, nil
			}
			if change.RunID != "" {
				changed[change.RunID] = struct{}{}
			}
			if change.Seq > seq {
				seq = change.Seq
				advanced = true
			}
		}
		if !advanced {
			break
		}
	}
	return changed, next, nil
}
