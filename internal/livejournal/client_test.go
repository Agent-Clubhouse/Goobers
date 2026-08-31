package livejournal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
