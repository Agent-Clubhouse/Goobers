package livejournal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// stubMinter records the run it was asked to mint for, so the test can assert
// the bearer is scoped to the batch rather than ambient.
type stubMinter struct {
	gotRun string
	gotTTL time.Duration
	token  string
	err    error
}

func (m *stubMinter) Mint(runID string, ttl time.Duration) (string, error) {
	m.gotRun, m.gotTTL = runID, ttl
	return m.token, m.err
}

// A worker holds the signing key, not any run's token. Without this the daemon
// refused every emit in a split deployment:
//
//	livejournal: emit refused (401 unauthenticated)
func TestHTTPEmitterMintsPerRunBearerWhenTokenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	minter := &stubMinter{token: "goobers-pod.minted"}
	e := &HTTPEmitter{BaseURL: srv.URL, Minter: minter}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-42"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if gotAuth != "Bearer goobers-pod.minted" {
		t.Fatalf("Authorization = %q, want the minted bearer", gotAuth)
	}
	// Scoped to THIS batch's run: a worker serves many runs concurrently and
	// must never present one run's authority while emitting for another.
	if minter.gotRun != "run-42" {
		t.Fatalf("minted for run %q, want the batch's run", minter.gotRun)
	}
	if minter.gotTTL <= 0 {
		t.Fatal("minted bearer must carry a bounded TTL")
	}
}

// An explicit token is the stage-pod posture and must keep winning, so this
// change cannot alter how a pod authenticates.
func TestHTTPEmitterPrefersExplicitTokenOverMinter(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	minter := &stubMinter{token: "minted"}
	e := &HTTPEmitter{BaseURL: srv.URL, Token: "explicit", Minter: minter}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if gotAuth != "Bearer explicit" {
		t.Fatalf("Authorization = %q, want the explicit token", gotAuth)
	}
	if minter.gotRun != "" {
		t.Fatal("minter must not be consulted when an explicit token is set")
	}
}

// With neither, no header at all — the loopback/no-auth posture, unchanged.
func TestHTTPEmitterSendsNoAuthorizationWithoutTokenOrMinter(t *testing.T) {
	present := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := &HTTPEmitter{BaseURL: srv.URL}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if present {
		t.Fatal("no token and no minter must send no Authorization header")
	}
}

// #4260: HTTPEmitter.Emit must survive the same transient network blips the
// surrender PUT does.
func TestHTTPEmitterRetriesTransientFailureThenSucceeds(t *testing.T) {
	shrinkRetryDelays(t)
	var attempts atomic.Int32
	const failures = 3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e := &HTTPEmitter{BaseURL: srv.URL, RetryDeadline: time.Second}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := attempts.Load(); got != failures+1 {
		t.Fatalf("server saw %d attempts, want %d", got, failures+1)
	}
}

func TestHTTPEmitterDoesNotRetry4xx(t *testing.T) {
	shrinkRetryDelays(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	e := &HTTPEmitter{BaseURL: srv.URL, RetryDeadline: time.Second}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"}); err == nil {
		t.Fatal("expected an error on a 401 response")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("server saw %d attempts, want exactly 1 (no retry on a 4xx)", got)
	}
}

func TestHTTPEmitterHonoursRetryDeadline(t *testing.T) {
	shrinkRetryDelays(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := &HTTPEmitter{BaseURL: srv.URL, RetryDeadline: 30 * time.Millisecond}
	start := time.Now()
	_, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once the retry deadline elapsed")
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want close to the 30ms retry deadline", elapsed)
	}
}

// The regression test #4260 calls for: kill the connection mid-emit (the
// daemon durably applies the batch, then the ack is lost) and confirm the
// retried emit neither loses the batch nor double-applies it. This drives
// the REAL Writer (livejournal_test.go's testWriter), not a stand-in, so it
// exercises the actual per-op idempotency-key dedup the retry relies on.
func TestHTTPEmitterRetryAfterLostAckDoesNotDoubleApply(t *testing.T) {
	shrinkRetryDelays(t)
	w, runsDir := testWriter(t)
	var puts atomic.Int32
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		puts.Add(1)
		var req EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		// Applied against the REAL writer — the durable write this
		// simulates as already-landed before the ack is lost.
		if _, err := w.Emit(r.Context(), req); err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}
		if first {
			first = false
			hijacker, ok := rw.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, herr := hijacker.Hijack()
			if herr != nil {
				t.Fatalf("hijack: %v", herr)
			}
			_ = conn.Close()
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(EmitResponse{})
	}))
	defer srv.Close()

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	batch := openBatch("run-lost-ack", started)
	e := &HTTPEmitter{BaseURL: srv.URL, RetryDeadline: time.Second}
	if _, err := e.Emit(context.Background(), batch); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("server saw %d emits, want 2 (the lost-ack write, then the client's retry)", got)
	}
	// Not lost: the batch's events landed exactly once, not zero times.
	// Not double-applied: exactly once, not twice — the retry's identical
	// per-op Key (openBatch derives keys deterministically) deduped against
	// the first, already-applied delivery.
	events := readEvents(t, runsDir, "run-lost-ack")
	if len(events) != 2 || events[0].Type != journal.EventRunStarted || events[1].Type != journal.EventStageStarted {
		t.Fatalf("events = %+v, want exactly the batch's 2 events applied once", events)
	}
}

// A mint failure must fail the emit, not silently send an unauthenticated
// request that the daemon would reject with a less specific error.
func TestHTTPEmitterFailsWhenMintFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not be sent when minting failed")
	}))
	defer srv.Close()

	e := &HTTPEmitter{BaseURL: srv.URL, Minter: &stubMinter{err: errors.New("no key")}}
	if _, err := e.Emit(context.Background(), EmitRequest{RunID: "run-1"}); err == nil {
		t.Fatal("expected the emit to fail when the bearer cannot be minted")
	}
}
