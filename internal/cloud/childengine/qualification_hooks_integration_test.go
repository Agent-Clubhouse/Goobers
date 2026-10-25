//go:build cloudintegration

package childengine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
)

// qualificationBarrier reports arrival before holding the caller. Release is
// idempotent; cancellation also unblocks an individual HTTP request.
type qualificationBarrier struct {
	arrived, released        chan struct{}
	arrivalOnce, releaseOnce sync.Once
}

func newQualificationBarrier() *qualificationBarrier {
	return &qualificationBarrier{arrived: make(chan struct{}), released: make(chan struct{})}
}
func (b *qualificationBarrier) Wait(ctx context.Context) error {
	select {
	case <-b.arrived:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *qualificationBarrier) Release() { b.releaseOnce.Do(func() { close(b.released) }) }
func (b *qualificationBarrier) hold(ctx context.Context) {
	b.arrivalOnce.Do(func() { close(b.arrived) })
	select {
	case <-b.released:
	case <-ctx.Done():
	}
}

type qualificationHTTPBarrier struct {
	match   func(*http.Request) bool
	claimed atomic.Bool
	barrier *qualificationBarrier
}
type qualificationHooks struct {
	mu     sync.Mutex
	http   []*qualificationHTTPBarrier
	stages map[string]*qualificationBarrier
}

// InstallHTTPBarrier holds the FIRST matching response after the real route
// completes its durable operation, but before the client sees the response.
// Never install a blocking inner handler: ServeHTTP holds routerMu there, and
// FailoverControl needs its write lock. This outer barrier holds neither lock.
func (h *qualificationHarness) InstallHTTPBarrier(match func(*http.Request) bool) *qualificationBarrier {
	b := newQualificationBarrier()
	h.hooks.mu.Lock()
	h.hooks.http = append(h.hooks.http, &qualificationHTTPBarrier{match: match, barrier: b})
	h.hooks.mu.Unlock()
	h.t.Cleanup(b.Release)
	return b
}

// StageBarrier creates a named barrier referenced by a serialized stage script.
// It holds every visit until released, including a replacement worker's retry.
func (h *qualificationHarness) StageBarrier(name string) *qualificationBarrier {
	h.hooks.mu.Lock()
	defer h.hooks.mu.Unlock()
	if h.hooks.stages == nil {
		h.hooks.stages = make(map[string]*qualificationBarrier)
	}
	if b := h.hooks.stages[name]; b != nil {
		return b
	}
	b := newQualificationBarrier()
	h.hooks.stages[name] = b
	h.t.Cleanup(b.Release)
	return b
}

// SetStageScript configures the next worker start, not an already-running worker.
func (h *qualificationHarness) SetStageScript(script qualificationStageScript) {
	if h.worker != nil {
		h.t.Fatal("set stage script before starting the worker")
	}
	h.stageScript = script
}

func (h *qualificationHarness) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const stagePrefix = "/__qualification/stage-barrier/"
	h.hooks.mu.Lock()
	if strings.HasPrefix(r.URL.Path, stagePrefix) {
		b := h.hooks.stages[strings.TrimPrefix(r.URL.Path, stagePrefix)]
		h.hooks.mu.Unlock()
		if b == nil {
			http.NotFound(w, r)
			return
		}
		b.hold(r.Context())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	barriers := append([]*qualificationHTTPBarrier(nil), h.hooks.http...)
	h.hooks.mu.Unlock()
	var held *qualificationBarrier
	for _, b := range barriers {
		if b.match(r) && b.claimed.CompareAndSwap(false, true) {
			held = b.barrier
			break
		}
	}
	serve := func(out http.ResponseWriter) {
		h.routerMu.RLock()
		defer h.routerMu.RUnlock()
		if h.handler == nil {
			http.Error(out, "control changing epoch", http.StatusServiceUnavailable)
			return
		}
		h.handler.ServeHTTP(out, r)
	}
	if held == nil {
		serve(w)
		return
	}
	response := httptest.NewRecorder()
	serve(response)
	held.hold(r.Context()) // routerMu has been released; takeover can finish.
	for name, values := range response.Header() {
		w.Header()[name] = values
	}
	w.WriteHeader(response.Code)
	_, _ = io.Copy(w, response.Body)
}
