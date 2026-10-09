package claimsclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyPlane fails the first n calls to each request with mode, then answers
// the claims-plane success body.
func flakyPlane(t *testing.T, n int32, mode string) (*HTTP, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= n {
			if mode == "reset" {
				panic(http.ErrAbortHandler)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"class_saturated","message":"too many concurrent mutation requests; retry shortly"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"released":[],"entries":[]}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "t", RunID: "run-1", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

var retryKey = Key{Gaggle: "g", Provider: "p", ExternalID: "1"}

func TestSaturationIsRetriedForEveryRoute(t *testing.T) {
	ctx := context.Background()
	routes := map[string]func(*HTTP) error{
		"acquire":     func(h *HTTP) error { _, _, err := h.ClaimScoped(ctx, retryKey, "run-1", "wf", time.Minute); return err },
		"release":     func(h *HTTP) error { return h.ReleaseScoped(ctx, retryKey, "run-1") },
		"release-all": func(h *HTTP) error { _, err := h.ReleaseAllForRun(ctx, "run-1"); return err },
		"list":        func(h *HTTP) error { _, err := h.ForRunAll(ctx, "run-1"); return err },
		"recover":     func(h *HTTP) error { _, err := h.RecoverStale(ctx); return err },
	}
	for name, call := range routes {
		client, calls := flakyPlane(t, 2, "saturated")
		if err := call(client); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if calls.Load() != 3 {
			t.Fatalf("%s: calls = %d, want 3", name, calls.Load())
		}
	}
}

func TestStreamResetRetriedOnlyForReplaySafeRoutes(t *testing.T) {
	ctx := context.Background()
	safe := map[string]func(*HTTP) error{
		"acquire": func(h *HTTP) error { _, _, err := h.ClaimScoped(ctx, retryKey, "run-1", "wf", time.Minute); return err },
		"release": func(h *HTTP) error { return h.ReleaseScoped(ctx, retryKey, "run-1") },
		"list":    func(h *HTTP) error { _, err := h.ForRunAll(ctx, "run-1"); return err },
		"recover": func(h *HTTP) error { _, err := h.RecoverStale(ctx); return err },
	}
	for name, call := range safe {
		client, calls := flakyPlane(t, 1, "reset")
		if err := call(client); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if calls.Load() != 2 {
			t.Fatalf("%s: calls = %d, want 2", name, calls.Load())
		}
	}
	client, calls := flakyPlane(t, 1, "reset")
	if _, err := client.ReleaseAllForRun(ctx, "run-1"); err == nil || !strings.Contains(err.Error(), "INTERNAL_ERROR") {
		t.Fatalf("release-all after an ambiguous reset must surface it, err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("release-all replayed after an ambiguous failure: calls = %d", calls.Load())
	}
}

func TestPermanentRefusalIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"run_mismatch","message":"no"}}`)
	}))
	defer server.Close()
	client, err := NewHTTP(HTTPConfig{BaseURL: server.URL, Token: "t", RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.ClaimScoped(context.Background(), retryKey, "run-1", "wf", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "run_mismatch") || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
