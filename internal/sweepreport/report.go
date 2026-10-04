// Package sweepreport records bounded, rate-limited daemon sweep errors.
package sweepreport

import (
	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/journal"
)

// Reporter rate-limits repeated sweep failures written to the instance journal.
type Reporter struct {
	log         *journal.InstanceLog
	code        string
	lastMessage string
	consecutive int
	reportEvery int
}

// New constructs a reporter with the caller's reporting interval.
func New(log *journal.InstanceLog, code string, reportEvery int) Reporter {
	return Reporter{log: log, code: code, reportEvery: reportEvery}
}

// Report records the first failure, periodic repeats, and changes after a reset.
func (r *Reporter) Report(err error) {
	if err == nil {
		r.lastMessage = ""
		r.consecutive = 0
		return
	}
	// Bound the persisted message regardless of how many entries a sweep
	// aggregated: this is the single write-boundary choke point guarding every
	// sweep reporter (stalled/trigger/claim/cancel/telemetry-retention) so a
	// giant record can never reach the scheduler journal and bloat the store
	// (#1414). Source-level bounding (boundedagg.Join in the stalled-run sweep)
	// is belt-and-suspenders on top of this.
	message := boundedagg.Bound(err.Error(), boundedagg.DefaultMaxBytes)
	if message != r.lastMessage {
		r.lastMessage = message
		r.consecutive = 1
	} else {
		r.consecutive++
	}
	if r.consecutive != 1 && (r.consecutive-1)%r.reportEvery != 0 {
		return
	}
	r.log.AppendBestEffort(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: r.code, Message: message},
		Runner: map[string]any{
			"consecutiveFailures": r.consecutive,
		},
	})
}
