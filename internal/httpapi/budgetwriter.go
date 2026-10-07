package httpapi

import (
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// budgetWriter answers a request whose budget expired before its handler wrote
// anything (#6890).
//
// withBudget arms the socket's write deadline at budget+margin. A handler
// blocked on a lock or a queue does not observe its context, so it never
// reaches budgetExceeded; the deadline then fires inside net/http and, over
// HTTP/2, resets the stream with INTERNAL_ERROR — a failure the daemon logs
// nowhere and the client cannot tell from a crash. The documented answer is
// 503 request_budget_exceeded, which clients classify as transient.
//
// net/http cannot end a response while the handler goroutine is still running,
// so the handler runs on its own goroutine (the http.TimeoutHandler design) and
// the serving goroutine answers and returns at the budget if the handler has
// not begun a response. The abandoned handler keeps its goroutine and its
// admission slot until it returns, and its writes are discarded. A handler that
// already started writing is left alone: a started response is the handler's to
// finish or the write deadline's to cut.
//
// The handler sees a private header map so the two goroutines never share one.
// The handler's headers reach the real writer at WriteHeader (or at return, for
// an implicit 200); every touch of the real writer holds mu.
type budgetWriter struct {
	underlying http.ResponseWriter

	mu        sync.Mutex
	header    http.Header
	started   bool // the handler began its response
	preempted bool // the budget answer was sent in its place
}

func newBudgetWriter(w http.ResponseWriter) *budgetWriter {
	return &budgetWriter{underlying: w, header: make(http.Header)}
}

// Unwrap lets http.ResponseController reach the connection (deadlines, flush).
func (b *budgetWriter) Unwrap() http.ResponseWriter { return b.underlying }

func (b *budgetWriter) Header() http.Header { return b.header }

// startLocked copies the handler's headers to the real writer once.
func (b *budgetWriter) startLocked() {
	if b.started {
		return
	}
	b.started = true
	dst := b.underlying.Header()
	for name, values := range b.header {
		dst[name] = append([]string(nil), values...)
	}
}

func (b *budgetWriter) WriteHeader(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.preempted {
		return
	}
	b.startLocked()
	b.underlying.WriteHeader(status)
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.preempted {
		return 0, http.ErrHandlerTimeout
	}
	b.startLocked()
	return b.underlying.Write(p)
}

func (b *budgetWriter) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.preempted {
		return
	}
	b.startLocked()
	if flusher, ok := b.underlying.(http.Flusher); ok {
		flusher.Flush()
	}
}

// finish hands over headers a handler set but never wrote (an implicit 200).
func (b *budgetWriter) finish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.preempted {
		b.startLocked()
	}
}

// preempt sends the 503 if the handler has not started. It reports whether it
// did.
func (b *budgetWriter) preempt() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started || b.preempted {
		return false
	}
	b.preempted = true
	header := b.underlying.Header()
	header.Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeError(b.underlying, http.StatusServiceUnavailable, "request_budget_exceeded",
		"the request exceeded its server-side time budget")
	return true
}

// serveWithBudgetAnswer runs handler on its own goroutine and, if the budget
// elapses before it has written anything, answers 503 request_budget_exceeded
// and logs the expiry with its route and how long the handler had run.
// handlerDone runs when the handler goroutine ends, however it ends — it is
// where the caller returns the admission slot the abandoned handler still holds.
func serveWithBudgetAnswer(logger *log.Logger, route string, budget time.Duration, w http.ResponseWriter, request *http.Request, handler http.HandlerFunc, handlerDone func()) {
	bw := newBudgetWriter(w)
	started := time.Now()
	finished := make(chan any, 1) // the handler's panic value, or nil
	go func() {
		defer handlerDone()
		defer func() { finished <- recover() }()
		handler(bw, request)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	var outcome any
	select {
	case outcome = <-finished:
	case <-timer.C:
		if bw.preempt() {
			if logger != nil {
				logger.Printf("request budget exceeded before the handler wrote a response: route=%s method=%s budget=%s elapsed=%s",
					route, request.Method, budget, time.Since(started).Round(time.Millisecond))
			}
			return
		}
		outcome = <-finished
	}
	if outcome != nil {
		panic(outcome) // http.ErrAbortHandler and real panics reach net/http as before
	}
	bw.finish()
}
