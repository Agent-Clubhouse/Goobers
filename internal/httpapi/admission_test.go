package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

// TestSaturatedClassShedsImmediately is the central choice in #1926.
//
// A saturated class that ACCEPTS work it cannot finish burns the request's whole
// budget waiting in a queue and then returns nothing — the caller waited the full
// budget for a failure it could have been told about instantly. Queue wait counts
// against the route budget, so accept-and-timeout is strictly worse than
// refusing: same outcome, plus a held connection and goroutine.
func TestSaturatedClassShedsImmediately(t *testing.T) {
	controller := newAdmissionController()
	limit := classLimits[apicontract.CostAggregate]

	releases := make([]func(), 0, limit)
	for i := 0; i < limit; i++ {
		release, ok := controller.admit(apicontract.CostAggregate)
		if !ok {
			t.Fatalf("admission %d of %d was refused below the ceiling", i+1, limit)
		}
		releases = append(releases, release)
	}

	// The next one must be refused, not queued.
	if _, ok := controller.admit(apicontract.CostAggregate); ok {
		t.Error("a request past the class ceiling was admitted; it would wait in a queue " +
			"that counts against its own budget and fail anyway")
	}

	// Releasing frees a slot.
	releases[0]()
	if _, ok := controller.admit(apicontract.CostAggregate); !ok {
		t.Error("a slot did not free after release")
	}
}

// TestClassesDoNotShareSlots is why the pools are per class rather than global.
//
// One global pool means an analytics burst blocks list reads — the specific
// symptom §9 names, and the reason the Overview's five queries could not run
// concurrently.
func TestClassesDoNotShareSlots(t *testing.T) {
	controller := newAdmissionController()

	// Saturate aggregates entirely.
	for i := 0; i < classLimits[apicontract.CostAggregate]; i++ {
		if _, ok := controller.admit(apicontract.CostAggregate); !ok {
			t.Fatal("failed to saturate the aggregate class")
		}
	}
	if _, ok := controller.admit(apicontract.CostAggregate); ok {
		t.Fatal("aggregate class is not actually saturated")
	}

	// Bounded reads must be unaffected.
	if _, ok := controller.admit(apicontract.CostBounded); !ok {
		t.Error("a bounded read was refused because the AGGREGATE class was saturated; " +
			"an analytics burst would block every list and navigation")
	}
}

// TestStreamsAreUnbounded pins that an SSE ceiling would cap open portal tabs.
//
// A subscription is long-lived by definition, so any finite ceiling is a limit
// on how many tabs a user may have open — which is not a resource decision
// anyone made deliberately.
func TestStreamsAreUnbounded(t *testing.T) {
	controller := newAdmissionController()
	for i := 0; i < 200; i++ {
		if _, ok := controller.admit(apicontract.CostStream); !ok {
			t.Fatalf("stream admission %d was refused; a ceiling on SSE caps open tabs", i+1)
		}
	}
}

// TestUnclassifiedRouteIsNotSilentlyRefused pins the fail-open direction.
//
// A missing class must not mean a zero ceiling — that would refuse every request
// to the route rather than merely leaving it unbounded. The contract test in
// apicontract is what prevents a route being unclassified in the first place;
// this is the belt to that braces.
func TestUnclassifiedRouteIsNotSilentlyRefused(t *testing.T) {
	controller := newAdmissionController()
	if _, ok := controller.admit(""); !ok {
		t.Error("an unclassified route was refused admission; a missing class must leave " +
			"the route unbounded, not silently reject every request to it")
	}
}

// TestReleaseIsIdempotent pins that a double release cannot free someone else's
// slot.
//
// The release runs from a deferred call on a path that can also write an error
// response; a second invocation freeing an unrelated slot would let the class
// exceed its ceiling silently.
func TestReleaseIsIdempotent(t *testing.T) {
	controller := newAdmissionController()
	release, ok := controller.admit(apicontract.CostAggregate)
	if !ok {
		t.Fatal("first admission refused")
	}
	release()
	release()

	// Occupancy must be zero, not negative-then-wrong.
	if got := controller.inFlight(apicontract.CostAggregate); got != 0 {
		t.Errorf("in-flight = %d after a double release, want 0", got)
	}
	// And the ceiling still holds.
	for i := 0; i < classLimits[apicontract.CostAggregate]; i++ {
		if _, ok := controller.admit(apicontract.CostAggregate); !ok {
			t.Fatalf("admission %d refused below the ceiling after a double release", i+1)
		}
	}
	if _, ok := controller.admit(apicontract.CostAggregate); ok {
		t.Error("the ceiling was exceeded after a double release")
	}
}

// TestAdmissionIsConcurrencySafe pins the obvious hazard, since the controller
// is shared across every in-flight request.
func TestAdmissionIsConcurrencySafe(t *testing.T) {
	controller := newAdmissionController()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := controller.admit(apicontract.CostBounded); ok {
				release()
			}
		}()
	}
	wg.Wait()
	if got := controller.inFlight(apicontract.CostBounded); got != 0 {
		t.Errorf("in-flight = %d after all releases, want 0", got)
	}
}

// TestRefusalCarriesRetryAfter pins that the client is told to retry rather than
// to give up.
//
// The class is saturated, not broken, and slots free as in-flight requests
// finish. A long value would make a transient burst look like an outage to a
// client that backs off on it.
func TestRefusalCarriesRetryAfter(t *testing.T) {
	response := httptest.NewRecorder()
	writeAdmissionRefusal(response, apicontract.CostAggregate)

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.Code)
	}
	if response.Header().Get(HeaderRetryAfterSeconds) == "" {
		t.Error("no Retry-After on an admission refusal; a client cannot tell a saturated " +
			"class from a dead server")
	}
	if code := errorCode(t, response); code != "class_saturated" {
		t.Errorf("code = %q, want class_saturated", code)
	}
}
