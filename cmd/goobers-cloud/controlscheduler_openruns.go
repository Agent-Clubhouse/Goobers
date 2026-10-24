package main

import (
	"context"
	"sort"
	"sync"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/schedstart"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// openRunParallel bounds the closed-run confirmations of one inventory.
const openRunParallel = 16

// runClosedFunc confirms a run's engine execution is closed
// (epoch.TemporalConfirmClosed): strongly consistent, unlike visibility.
type runClosedFunc func(ctx context.Context, runID string) (bool, error)

// openRunInputs are the durable sources of the runs holding a scheduler slot.
type openRunInputs struct {
	// Pins are the start fence's run pins not yet marked terminal. That is runs
	// that MAY be open: terminal_at is set only after ReplayRetention, so a
	// recently closed run is still listed and is confirmed against Temporal.
	Pins []acceptance.RunPins
	// Uncertain are acceptance rows whose start outcome is unresolved
	// ('dispatching'); they hold a slot until the engine says otherwise, because
	// admitting over them could exceed MaxConcurrentRuns.
	Uncertain []triggerqueue.Record
	Closed    runClosedFunc
}

// collectOpenRuns merges pinned runs the engine does not confirm closed with
// unresolved dispatching starts, one entry per run. A confirmation that fails is
// treated as open: the watcher keeps retrying and the slot stays held, which is
// the conservative direction (under-admit, never over-admit).
func collectOpenRuns(ctx context.Context, in openRunInputs) ([]schedstart.OpenRun, error) {
	type result struct {
		run  schedstart.OpenRun
		open bool
	}
	results := make([]result, len(in.Pins))
	slots := make(chan struct{}, openRunParallel)
	var wg sync.WaitGroup
	for i, pins := range in.Pins {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			closed := false
			if in.Closed != nil {
				confirmed, err := in.Closed(ctx, pins.RunID)
				closed = err == nil && confirmed
			}
			results[i] = result{
				run:  schedstart.OpenRun{Identity: localscheduler.WorkflowIdentity{Gaggle: pins.Gaggle, Workflow: pins.Workflow}, RunID: pins.RunID},
				open: !closed,
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []schedstart.OpenRun
	for _, r := range results {
		if r.open && !seen[r.run.RunID] {
			seen[r.run.RunID] = true
			out = append(out, r.run)
		}
	}
	for _, record := range in.Uncertain {
		run, ok := holderFromRecord(record)
		if !ok || run.RunID == "" || seen[run.RunID] {
			continue
		}
		seen[run.RunID] = true
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// uncertainStarts pages every retained acceptance row still 'dispatching'.
func uncertainStarts(ctx context.Context, pager interface {
	RetainedPage(context.Context, string, int) ([]triggerqueue.Record, error)
}) ([]triggerqueue.Record, error) {
	var out []triggerqueue.Record
	after := ""
	for {
		page, err := pager.RetainedPage(ctx, after, 100)
		if err != nil {
			return nil, err
		}
		for _, record := range page {
			if record.State == triggerqueue.Dispatching {
				out = append(out, record)
			}
			after = record.ID
		}
		if len(page) < 100 {
			return out, nil
		}
	}
}
