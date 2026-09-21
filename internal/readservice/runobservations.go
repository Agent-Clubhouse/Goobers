package readservice

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

type runOperationalObservations struct {
	retryBackoff   readmodel.RetryBackoffState
	activity       readmodel.StageActivity
	engineFallback *readmodel.EngineFallback
	requiredMCP    *readmodel.RequiredMCPState
}

func (o *runOperationalObservations) after(event journal.Event) {
	o.activity = o.activity.After(event)
	o.retryBackoff = o.retryBackoff.After(event)
	o.engineFallback = o.engineFallback.After(event)
	o.requiredMCP = o.requiredMCP.After(event)
}
