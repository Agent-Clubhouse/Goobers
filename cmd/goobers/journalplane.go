package main

import (
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readmodel/intake"
)

// journalplane.go wires the daemon side of the write API's journal plane
// (distributed-state-and-coordination.md §7/§8, DS4/DS5): the live journal
// writer that authors engine-run journals from emitted events, the HTTP
// service in front of it, and the divergence channel the demoted repair
// projection files into.

// liveJournalDivergenceCode is the stable instance-journal error code a
// live-vs-projected divergence (DS5) is filed under — the named channel the
// #2871 parity ledger reads.
const liveJournalDivergenceCode = "live_journal_divergence"

// liveJournalIdleClose is how long a live run's journal handle may sit
// without an emission before the writer releases it (the run-dir lock
// included), so the stalled-run sweep — whose default silence threshold is
// 45 minutes and never shorter than this — can terminalize a wedged run
// instead of timing out against the writer's lock. A later emission
// transparently reopens.
const liveJournalIdleClose = 10 * time.Minute

// newLiveJournalWriter builds the daemon's live journal writer when the
// engine is configured — the same gate as the projection loop, because the
// writer and the demoted reconciler are two halves of one authority story
// (DS4/DS5). Returns nil (not an error) for an instance with no engine.
//
// No span source is wired yet: span ops degrade to span_unavailable error
// events, exactly as the daemon's history projection does today — the
// blobstore wiring for both is the recorded #3515/#3513 open point.
func newLiveJournalWriter(l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, watermarks *intake.Store, instanceLog *journal.InstanceLog) (*livejournal.Writer, error) {
	if cfg == nil || !cfg.EngineProjectionEnabled() {
		return nil, nil
	}
	runsDirs := make(map[string]string)
	for _, gaggle := range configuredGaggleNames(set) {
		runsDirs[gaggle] = l.ForGaggle(gaggle).RunsDir()
	}
	opts := []livejournal.Option{}
	if observer := runIntakeObserver(watermarks, instanceLog); observer != nil {
		// The same read-model intake the local runner notifies per append —
		// which is what makes a live engine run's stage transitions reach SSE
		// and the portal through the existing machinery, mid-run.
		opts = append(opts, livejournal.WithObserver(observer))
	}
	return livejournal.NewWriter(func(gaggle string) (string, bool) {
		dir, ok := runsDirs[gaggle]
		return dir, ok
	}, opts...)
}

// liveJournalDivergenceReporter files one DS5 divergence (or a visible
// backfill annotation) into the instance journal under the stable code.
// Best-effort like the claims plane's own annotations: the reconcile outcome
// is already decided by the caller.
func liveJournalDivergenceReporter(log *journal.InstanceLog) engine.DivergenceReporter {
	if log == nil {
		return nil
	}
	return func(runID, detail string) {
		_ = log.Append(journal.Event{
			Type:  journal.EventError,
			RunID: runID,
			Error: &journal.ErrorDetail{Code: liveJournalDivergenceCode, Message: detail},
		})
	}
}
