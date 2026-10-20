package terminalreport

import (
	"sync"
	"time"
)

const (
	reportCallTimeout     = 30 * time.Second
	reportLeaseSkewMargin = 30 * time.Second
	reportLeaseDuration   = reportCallTimeout + reportLeaseSkewMargin
)

// A JSON lease alone cannot exclude a still-live publisher after a forward
// wall-clock adjustment. The process-wide guard covers fresh Queue values,
// separate outbox directories and generations for the same provider target.
// Admission remains independent; only remote delivery is single-flight.
var reportFlights sync.Map

func enterFlight(key string) (func(), bool) {
	if _, loaded := reportFlights.LoadOrStore(key, struct{}{}); loaded {
		return nil, false
	}
	return func() { reportFlights.Delete(key) }, true
}
