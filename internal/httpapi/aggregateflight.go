package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

type aggregateFlightGroup struct {
	mu      sync.Mutex
	flights map[string]*aggregateFlight
}

type aggregateFlight struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	finished bool
	result   aggregateFlightResult
}

type aggregateFlightResult struct {
	response bufferedResponse
	panic    any
}

type bufferedResponse struct {
	header http.Header
	status int
	body   []byte
}

type responseBuffer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{header: make(http.Header)}
}

func (w *responseBuffer) Header() http.Header {
	return w.header
}

func (w *responseBuffer) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
}

func (w *responseBuffer) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func (w *responseBuffer) snapshot() bufferedResponse {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	return bufferedResponse{
		header: w.header.Clone(),
		status: status,
		body:   bytes.Clone(w.body.Bytes()),
	}
}

func (r *Router) serveAggregate(
	route apicontract.Route,
	handler http.HandlerFunc,
	w http.ResponseWriter,
	request *http.Request,
) {
	if err := request.Context().Err(); err != nil {
		clientCancelled(w, err)
		return
	}
	if budget, bounded := routeBudget(route.ID); bounded {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget + writeDeadlineMargin))
	}
	result, err := r.aggregateReads.do(request.Context(), aggregateRequestKey(route, request), func(sharedContext context.Context) aggregateFlightResult {
		buffer := newResponseBuffer()
		sharedRequest := request.Clone(sharedContext)
		out := aggregateFlightResult{}
		func() {
			defer func() {
				out.panic = recover()
			}()
			r.serveAdmitted(route, handler, buffer, sharedRequest)
		}()
		out.response = buffer.snapshot()
		return out
	})
	if err != nil {
		clientCancelled(w, err)
		return
	}
	if result.panic != nil {
		panic(result.panic)
	}
	replayBufferedResponse(w, result.response)
}

func (g *aggregateFlightGroup) do(
	ctx context.Context,
	key string,
	run func(context.Context) aggregateFlightResult,
) (aggregateFlightResult, error) {
	g.mu.Lock()
	if g.flights == nil {
		g.flights = make(map[string]*aggregateFlight)
	}
	flight := g.flights[key]
	if flight == nil {
		// The shared read keeps running while any identical caller still needs
		// it, but is cancelled when the last waiter leaves. Completed responses
		// are removed immediately; this is coalescing, not a response cache.
		sharedContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &aggregateFlight{done: make(chan struct{}), cancel: cancel}
		g.flights[key] = flight
		go g.run(key, flight, sharedContext, run)
	}
	flight.waiters++
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.leave(key, flight)
		return aggregateFlightResult{}, ctx.Err()
	case <-flight.done:
		return flight.result, nil
	}
}

func (g *aggregateFlightGroup) run(
	key string,
	flight *aggregateFlight,
	ctx context.Context,
	run func(context.Context) aggregateFlightResult,
) {
	result := run(ctx)
	g.mu.Lock()
	flight.result = result
	flight.finished = true
	if g.flights[key] == flight {
		delete(g.flights, key)
	}
	close(flight.done)
	g.mu.Unlock()
	flight.cancel()
}

func (g *aggregateFlightGroup) leave(key string, flight *aggregateFlight) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if flight.finished {
		return
	}
	flight.waiters--
	if flight.waiters == 0 {
		if g.flights[key] == flight {
			delete(g.flights, key)
		}
		flight.cancel()
	}
}

func aggregateRequestKey(route apicontract.Route, request *http.Request) string {
	var key strings.Builder
	key.WriteString(string(route.ID))
	key.WriteByte(0)
	key.WriteString(request.URL.RequestURI())
	if principal, ok := PrincipalFromRequest(request); ok {
		key.WriteByte(0)
		key.WriteString(principal.Issuer)
		key.WriteByte(0)
		key.WriteString(principal.Subject)
		for _, role := range principal.Roles {
			key.WriteByte(0)
			key.WriteString(string(role))
		}
		key.WriteByte(0)
		key.WriteString(strings.Join(principal.Scopes, ","))
	}
	return key.String()
}

func replayBufferedResponse(w http.ResponseWriter, response bufferedResponse) {
	for name, values := range response.header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}
