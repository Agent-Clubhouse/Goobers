package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	controller := newAdmissionController(classLimits)
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
	controller := newAdmissionController(classLimits)

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
	controller := newAdmissionController(classLimits)
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
	controller := newAdmissionController(classLimits)
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
	controller := newAdmissionController(classLimits)
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
	controller := newAdmissionController(classLimits)
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

	if response.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", response.Code)
	}
	retryAfter := response.Header().Get(HeaderRetryAfterSeconds)
	if retryAfter == "" {
		t.Error("no Retry-After on an admission refusal; a client cannot tell a saturated " +
			"class from a dead server")
	} else if seconds, err := strconv.Atoi(retryAfter); err != nil || seconds < 1 {
		t.Errorf("Retry-After = %q, want a positive standards-compatible delay-seconds value", retryAfter)
	}
	if code := errorCode(t, response); code != "class_saturated" {
		t.Errorf("code = %q, want class_saturated", code)
	}
}

func TestNonAggregateRefusalRetainsServiceUnavailable(t *testing.T) {
	response := httptest.NewRecorder()
	writeAdmissionRefusal(response, apicontract.CostBounded)

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.Code)
	}
	if code := errorCode(t, response); code != "class_saturated" {
		t.Errorf("code = %q, want class_saturated", code)
	}
}

// TestSequentialAggregateRequestsNeverSaturate is the #6926 regression: a
// budgeted request returned its admission slot from the handler goroutine
// AFTER signalling completion, so ServeHTTP could return while the slot was
// still held and the next sequential request on the same handler got 429.
// With the aggregate ceiling at 1, any lag fails the very next request.
func TestSequentialAggregateRequestsNeverSaturate(t *testing.T) {
	newHandler := func() (*Router, http.Handler) {
		router, err := newRouter(NullAuthenticator{}, authorizerFunc(func(*http.Request) error { return nil }))
		if err != nil {
			t.Fatal(err)
		}
		router.Handle(apicontract.RouteTelemetryImplementationOutcomes, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		// After Handle, which lazily installs the production limits.
		router.admission = newAdmissionController(map[apicontract.CostClass]int{apicontract.CostAggregate: 1})
		return router, router.Handler()
	}
	first, firstHandler := newHandler()
	second, secondHandler := newHandler()

	held, ok := second.admission.admit(apicontract.CostAggregate)
	if !ok {
		t.Fatal("could not occupy the second handler's only aggregate slot")
	}
	defer held()

	for i := 0; i < 200; i++ {
		response := httptest.NewRecorder()
		firstHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TelemetryImplementationOutcomesPath, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("sequential request %d status = %d, body = %s", i+1, response.Code, response.Body)
		}
		if got := first.admission.inFlight(apicontract.CostAggregate); got != 0 {
			t.Fatalf("aggregate in-flight = %d after request %d returned, want 0", got, i+1)
		}
	}

	response := httptest.NewRecorder()
	secondHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TelemetryImplementationOutcomesPath, nil))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated second handler status = %d, want 429", response.Code)
	}
}

// TestBudgetAnswerReleasesBeforeReturning pins the ordering behind #6926
// directly: handlerDone must have finished by the time serveWithBudgetAnswer
// returns for a handler that completed within its budget. The slow handlerDone
// makes an asynchronous release lose the race every time.
func TestBudgetAnswerReleasesBeforeReturning(t *testing.T) {
	var released atomic.Bool
	handlerDone := func() {
		time.Sleep(20 * time.Millisecond)
		released.Store(true)
	}
	serveWithBudgetAnswer(nil, "test", time.Minute, time.Minute, httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, handlerDone)
	if !released.Load() {
		t.Fatal("serveWithBudgetAnswer returned before handlerDone released the admission slot")
	}
}
