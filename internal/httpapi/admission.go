package httpapi

import (
	"net/http"
	"sync"

	"github.com/goobers/goobers/internal/apicontract"
)

// Per-class admission control (#1926 part ii, design §7.1, §9).
//
// # Shed at admission, not accept-and-timeout
//
// A saturated class that ACCEPTS work it cannot finish burns the request's whole
// budget in the queue and then returns nothing. The caller waited the full
// budget for a failure it could have been told about immediately.
//
// Queue wait counts against the route budget (the budget clock starts when the
// request arrives, not when a worker picks it up), so accept-and-timeout is
// strictly worse than refusing: same outcome for the caller, plus the server
// held a connection and a goroutine for it.
//
// Overflow is therefore a FAST 503 with Retry-After.
//
// # Why per class rather than one global pool
//
// The classes have different cost profiles and different saturation causes. One
// global pool means an analytics burst blocks list reads — which is the specific
// symptom §9 names, and the reason the Overview's five queries could not run
// concurrently.

// classLimits are the concurrent-request ceilings per cost class.
//
// Bounded reads get the most because they are the cheapest and the most
// frequent: the Runs page, the Overview's fan-out, every navigation. Aggregates
// get the fewest because each one is the most expensive thing the read path
// does, and because two concurrent analytics queries already saturate the
// reader pool.
//
// Blob is separate and generous: those requests are dominated by transfer time,
// not by server work, so a slow client holding a slot costs almost nothing in
// CPU and should not consume a bounded-read slot.
//
// Stream is unlimited by construction — an SSE subscription is long-lived by
// definition, and a ceiling on it would cap the number of open portal tabs.
var classLimits = map[apicontract.CostClass]int{
	apicontract.CostBounded:   16,
	apicontract.CostSingleRun: 8,
	apicontract.CostAggregate: 2,
	apicontract.CostBlob:      8,
	apicontract.CostMutation:  4,
}

// admissionController bounds concurrency per cost class.
type admissionController struct {
	mu    sync.Mutex
	slots map[apicontract.CostClass]chan struct{}
}

func newAdmissionController() *admissionController {
	slots := make(map[apicontract.CostClass]chan struct{}, len(classLimits))
	for class, limit := range classLimits {
		slots[class] = make(chan struct{}, limit)
	}
	return &admissionController{slots: slots}
}

// admit takes a slot for a route's class, or reports that the class is
// saturated.
//
// Non-blocking by design. A blocking acquire IS accept-and-timeout wearing a
// different name: the caller waits in a queue that counts against its budget and
// then fails anyway.
func (a *admissionController) admit(class apicontract.CostClass) (release func(), ok bool) {
	if class == apicontract.CostStream || class == "" {
		// Streams are unbounded (see classLimits), and an unclassified route
		// gets no ceiling rather than a zero one — a missing class must not
		// silently refuse every request to that route. The contract test in
		// apicontract is what stops a route being unclassified in the first
		// place.
		return func() {}, true
	}

	a.mu.Lock()
	slot, known := a.slots[class]
	a.mu.Unlock()
	if !known {
		return func() {}, true
	}

	select {
	case slot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slot }) }, true
	default:
		return nil, false
	}
}

// inFlight reports current occupancy for a class, for diagnostics.
func (a *admissionController) inFlight(class apicontract.CostClass) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if slot, ok := a.slots[class]; ok {
		return len(slot)
	}
	return 0
}

// writeAdmissionRefusal sends the fast 503.
//
// Retry-After is short: the class is saturated, not broken, and slots free as
// in-flight requests finish. A long value would make a transient burst look like
// an outage to a client that backs off on it.
func writeAdmissionRefusal(w http.ResponseWriter, class apicontract.CostClass) {
	w.Header().Set(HeaderRetryAfterSeconds, "1")
	writeError(w, http.StatusServiceUnavailable, "class_saturated",
		"too many concurrent "+string(class)+" requests; retry shortly")
}
