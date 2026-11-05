package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type shutdownProbe struct {
	http.Handler
	authenticated bool
	stopped       bool
}

func (p *shutdownProbe) authenticatedTransport() bool { return p.authenticated }
func (p *shutdownProbe) shutdown()                    { p.stopped = true }

func TestWrapWithRoutePreservesTransportEvidenceAndLifecycle(t *testing.T) {
	inner := &shutdownProbe{Handler: http.NotFoundHandler(), authenticated: true}
	route := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	wrapped := WrapWithRoute(inner, "/hook", route)
	if !handlerAuthenticated(wrapped) {
		t.Fatal("wrapping hid the inner handler's authenticated transport")
	}
	if handlerAuthenticated(WrapWithRoute(&shutdownProbe{Handler: http.NotFoundHandler()}, "/hook", route)) {
		t.Fatal("wrapping made an unauthenticated handler look authenticated")
	}
	wrapped.(interface{ shutdown() }).shutdown()
	if !inner.stopped {
		t.Fatal("shutdown did not reach the inner handler")
	}
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hook", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("route status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hook/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("only the exact path is routed, got %d", rec.Code)
	}
}
