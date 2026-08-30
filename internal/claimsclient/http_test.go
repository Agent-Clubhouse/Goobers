package claimsclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// fakePlane is a claims plane that records every request and answers from a
// per-path script.
type fakePlane struct {
	mu       sync.Mutex
	requests []fakePlaneRequest
	answer   func(path string, body map[string]any) (int, any)
}

type fakePlaneRequest struct {
	path   string
	bearer string
	body   map[string]any
}

func (p *fakePlane) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, fakePlaneRequest{
		path:   r.URL.Path,
		bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		body:   body,
	})
	p.mu.Unlock()
	status, response := p.answer(r.URL.Path, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func (p *fakePlane) recorded() []fakePlaneRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]fakePlaneRequest(nil), p.requests...)
}

func newFakePlane(t *testing.T, answer func(path string, body map[string]any) (int, any)) (*fakePlane, *HTTP) {
	t.Helper()
	plane := &fakePlane{answer: answer}
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	t.Cleanup(server.Close)
	client, err := NewHTTP(HTTPConfig{
		BaseURL: server.URL, Token: "claims-token", RunID: "run-1",
		MergeLockPoll: 5 * time.Millisecond, MergeLockLease: 60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plane, client
}

func TestNewHTTPRequiresBaseURLTokenAndRun(t *testing.T) {
	for name, cfg := range map[string]HTTPConfig{
		"no base url": {Token: "t", RunID: "r"},
		"no token":    {BaseURL: "http://d", RunID: "r"},
		"no run":      {BaseURL: "http://d", Token: "t"},
	} {
		if _, err := NewHTTP(cfg); err == nil {
			t.Errorf("%s: NewHTTP accepted %+v", name, cfg)
		}
	}
}

// TestHTTPClaimScopedWire pins acquire's wire shape: the contract path, the
// bearer, the namespaced key, the run/workflow, the lease in whole seconds;
// and the answer mapping: ok with the caller as holder, refused with the
// plane's holder.
func TestHTTPClaimScopedWire(t *testing.T) {
	plane, client := newFakePlane(t, func(_ string, body map[string]any) (int, any) {
		if body["runId"] == "run-1" {
			return http.StatusOK, map[string]any{"ok": true, "expiresAt": time.Now().Add(time.Hour)}
		}
		return http.StatusOK, map[string]any{"ok": false, "holder": "run-1"}
	})
	ctx := context.Background()
	key := Key{Gaggle: "g", Provider: "github", ExternalID: "7"}
	ok, holder, err := client.ClaimScoped(ctx, key, "run-1", "implementation", 90*time.Second+time.Millisecond)
	if err != nil || !ok || holder != "run-1" {
		t.Fatalf("acquire = %v, %q, %v", ok, holder, err)
	}
	ok, holder, err = client.ClaimScoped(ctx, key, "run-2", "implementation", time.Hour)
	if err != nil || ok || holder != "run-1" {
		t.Fatalf("contended acquire = %v, %q, %v; want refused naming run-1", ok, holder, err)
	}
	got := plane.recorded()
	if len(got) != 2 || got[0].path != apicontract.ClaimAcquirePath || got[0].bearer != "claims-token" {
		t.Fatalf("recorded = %+v", got)
	}
	want := map[string]any{"gaggle": "g", "provider": "github", "itemId": "7", "runId": "run-1", "workflow": "implementation", "leaseSeconds": float64(91)}
	for field, value := range want {
		if got[0].body[field] != value {
			t.Errorf("acquire body %s = %v, want %v (body %v)", field, got[0].body[field], value, got[0].body)
		}
	}

	if _, _, err := client.ClaimScoped(ctx, Key{ExternalID: "legacy"}, "run-1", "w", time.Hour); !errors.Is(err, ErrLegacyKeyOverPlane) {
		t.Fatalf("legacy key over the plane: err = %v", err)
	}
	if err := client.ReleaseScoped(ctx, Key{ExternalID: "legacy"}, "run-1"); !errors.Is(err, ErrLegacyKeyOverPlane) {
		t.Fatalf("legacy release over the plane: err = %v", err)
	}
	if _, err := client.ListNamespace(ctx, "", ""); !errors.Is(err, ErrLegacyKeyOverPlane) {
		t.Fatalf("legacy namespace listing over the plane: err = %v", err)
	}
	if len(plane.recorded()) != 2 {
		t.Fatal("a legacy-key call reached the plane")
	}
}

// TestHTTPErrorEnvelope pins that a refusal surfaces as a typed Error
// carrying the plane's code — a containment refusal and a transport fault
// must not read the same.
func TestHTTPErrorEnvelope(t *testing.T) {
	_, client := newFakePlane(t, func(string, map[string]any) (int, any) {
		return http.StatusForbidden, map[string]any{"error": map[string]any{"code": "run_mismatch", "message": "not your run"}}
	})
	_, _, err := client.ClaimScoped(context.Background(), Key{Gaggle: "g", Provider: "p", ExternalID: "1"}, "run-2", "w", time.Hour)
	var planeErr *Error
	if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden || planeErr.Code != "run_mismatch" {
		t.Fatalf("err = %v, want a *Error with 403 run_mismatch", err)
	}
	if !strings.Contains(err.Error(), "not your run") {
		t.Fatalf("error text %q lost the plane's message", err)
	}
}

// TestHTTPReleaseAndListWire pins the release (single and all-for-run) and
// list (run and namespace scope) wire shapes and their decoding.
func TestHTTPReleaseAndListWire(t *testing.T) {
	released := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	plane, client := newFakePlane(t, func(path string, body map[string]any) (int, any) {
		switch path {
		case apicontract.ClaimReleasePath:
			if body["itemId"] == nil {
				return http.StatusOK, map[string]any{"ok": true, "released": []Entry{{ItemID: "7", Gaggle: "g", Provider: "github", ExternalID: "7", RunID: "run-1"}}}
			}
			return http.StatusOK, map[string]any{"ok": true}
		case apicontract.ClaimListPath:
			return http.StatusOK, map[string]any{
				"entries": []Entry{{ItemID: "7", Gaggle: "g", Provider: "github", ExternalID: "7", RunID: "run-1"}},
				"history": []Entry{{ItemID: "8", Gaggle: "g", Provider: "github", ExternalID: "8", RunID: "run-0", ReleasedAt: &released}},
			}
		}
		return http.StatusNotFound, map[string]any{}
	})
	ctx := context.Background()
	if err := client.ReleaseScoped(ctx, Key{Gaggle: "g", Provider: "github", ExternalID: "7"}, "run-1"); err != nil {
		t.Fatal(err)
	}
	all, err := client.ReleaseAllForRun(ctx, "run-1")
	if err != nil || len(all) != 1 || all[0].ItemID != "7" {
		t.Fatalf("ReleaseAllForRun = %+v, %v", all, err)
	}
	held, err := client.ForRunAll(ctx, "run-1")
	if err != nil || len(held) != 1 || held[0].RunID != "run-1" {
		t.Fatalf("ForRunAll = %+v, %v", held, err)
	}
	listing, err := client.ListNamespace(ctx, "g", "github")
	if err != nil || len(listing.Entries) != 1 || len(listing.History) != 1 || listing.History[0].ReleasedAt == nil {
		t.Fatalf("ListNamespace = %+v, %v", listing, err)
	}

	got := plane.recorded()
	if len(got) != 4 {
		t.Fatalf("recorded %d requests: %+v", len(got), got)
	}
	if got[0].body["itemId"] != "7" || got[0].body["gaggle"] != "g" {
		t.Errorf("single release body = %v", got[0].body)
	}
	if _, hasItem := got[1].body["itemId"]; hasItem || got[1].body["runId"] != "run-1" {
		t.Errorf("release-all body = %v, want runId only", got[1].body)
	}
	if got[2].path != apicontract.ClaimListPath || got[2].body["scope"] != scopeRun || got[2].body["runId"] != "run-1" {
		t.Errorf("ForRunAll list body = %v", got[2].body)
	}
	if got[3].body["scope"] != scopeNamespace || got[3].body["includeHistory"] != true || got[3].body["runId"] != "run-1" || got[3].body["gaggle"] != "g" {
		t.Errorf("ListNamespace body = %v, want namespace scope with history contained to run-1", got[3].body)
	}
}

// TestHTTPMergeLockPollsRenewsAndReleases pins the lease shape of the merge
// window: acquire is retried until held (the refusal's holder is the wait
// signal), the lease is renewed while fn runs, and the lock is released on
// the way out — including when fn fails.
func TestHTTPMergeLockPollsRenewsAndReleases(t *testing.T) {
	var mu sync.Mutex
	refusals := 2
	plane, client := newFakePlane(t, func(path string, body map[string]any) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		switch path {
		case apicontract.ClaimAcquirePath:
			if refusals > 0 {
				refusals--
				return http.StatusOK, map[string]any{"ok": false, "holder": "run-0"}
			}
			return http.StatusOK, map[string]any{"ok": true}
		case apicontract.ClaimRenewPath, apicontract.ClaimReleasePath:
			return http.StatusOK, map[string]any{"ok": true}
		}
		return http.StatusNotFound, map[string]any{}
	})
	lock := MergeLock{Key: MergeLockKey("g", "github", "acme", "web"), RunID: "run-1", Workflow: "merge-review"}
	windowErr := errors.New("merge refused")
	err := client.MergeLock(context.Background(), lock, func(context.Context) error {
		time.Sleep(3 * client.cfg.MergeLockLease / 3 * 2)
		return windowErr
	})
	if !errors.Is(err, windowErr) {
		t.Fatalf("MergeLock err = %v, want fn's error passed through", err)
	}
	var acquires, renews, releases int
	for _, request := range plane.recorded() {
		if request.body["itemId"] != "merge-lock/acme/web" || request.body["runId"] != "run-1" {
			t.Fatalf("merge-lock request on the wrong item/run: %+v", request)
		}
		switch request.path {
		case apicontract.ClaimAcquirePath:
			acquires++
		case apicontract.ClaimRenewPath:
			renews++
		case apicontract.ClaimReleasePath:
			releases++
		}
	}
	if acquires != 3 || renews == 0 || releases != 1 {
		t.Fatalf("acquires = %d (want 3: two refusals, then held), renews = %d (want > 0), releases = %d (want 1)", acquires, renews, releases)
	}

	// A waiting claimant gives up when its context ends, naming the holder.
	mu.Lock()
	refusals = 1 << 30
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	ran := false
	err = client.MergeLock(ctx, lock, func(context.Context) error { ran = true; return nil })
	if err == nil || ran || !strings.Contains(err.Error(), "held by run run-0") {
		t.Fatalf("waiting MergeLock: err = %v, ran = %v", err, ran)
	}
	if err := client.MergeLock(context.Background(), MergeLock{Key: lock.Key}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("MergeLock without a run id was accepted")
	}
}

// TestHTTPMergeLockFailsClosedOnLostLease is SHOULD #3: a renewal that comes
// back definitively refused (ok=false, not a transport error) must cancel
// fn's context with errMergeLeaseLost — and, if fn does not itself observe
// that cancellation before returning, MergeLock must still surface the loss
// as its own error rather than let a nil fn error read as "the window held".
func TestHTTPMergeLockFailsClosedOnLostLease(t *testing.T) {
	var mu sync.Mutex
	renewed := false
	plane, client := newFakePlane(t, func(path string, body map[string]any) (int, any) {
		mu.Lock()
		defer mu.Unlock()
		switch path {
		case apicontract.ClaimAcquirePath:
			return http.StatusOK, map[string]any{"ok": true}
		case apicontract.ClaimRenewPath:
			renewed = true
			// Definitive refusal: the daemon says another run now holds it.
			return http.StatusOK, map[string]any{"ok": false, "holder": "run-thief"}
		case apicontract.ClaimReleasePath:
			return http.StatusOK, map[string]any{"ok": true}
		}
		return http.StatusNotFound, map[string]any{}
	})
	client.cfg.MergeLockLease = 30 * time.Millisecond
	lock := MergeLock{Key: MergeLockKey("g", "github", "acme", "web"), RunID: "run-1", Workflow: "merge-review"}

	var sawCancel bool
	var fnCtxErr error
	err := client.MergeLock(context.Background(), lock, func(fnCtx context.Context) error {
		// fn does its own work first (as mergepr.go's closure does — it
		// never returns fnCtx.Err() itself), then, if given long enough for
		// the definitive-refusal renewal above to land, observes the
		// cancellation on a later ctx-aware wait — never a raw sleep past
		// deadline.
		select {
		case <-fnCtx.Done():
			sawCancel = true
			fnCtxErr = context.Cause(fnCtx)
		case <-time.After(2 * time.Second):
		}
		return nil
	})
	if !renewed {
		t.Fatal("test did not exercise a renewal at all")
	}
	if !sawCancel {
		t.Fatal("fn's context was never cancelled after the lease was definitively lost")
	}
	if !errors.Is(fnCtxErr, errMergeLeaseLost) {
		t.Fatalf("fn's context.Cause = %v, want errMergeLeaseLost", fnCtxErr)
	}
	// fn itself returned nil (it does not read its own context in
	// mergepr.go's shape) — MergeLock must still fail closed rather than
	// report success for a window it knows was surrendered mid-flight.
	if err == nil || !errors.Is(err, errMergeLeaseLost) {
		t.Fatalf("MergeLock err = %v, want it to surface the lost lease even though fn returned nil", err)
	}
	var releases int
	for _, request := range plane.recorded() {
		if request.path == apicontract.ClaimReleasePath {
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1: the window is still handed back on the way out", releases)
	}
}
