package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
)

func TestWorkerDigestMintFailureReportsUnavailableAndRecovers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"digest":"sha256:original"}`)
	}))
	t.Cleanup(server.Close)
	var available atomic.Bool
	messages := make(chan string, 16)
	seams := &workerSeams{logf: func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }}
	seams.snapshot.Store(&workerConfigSnapshot{digest: "sha256:original"})
	watcher := startWorkerDivergenceWatcher(context.Background(), seams, server.Client(), server.URL, func() (string, error) {
		if !available.Load() {
			return "", errors.New("credential unavailable")
		}
		return "restored", nil
	}, 10*time.Millisecond)
	t.Cleanup(watcher.Stop)
	select {
	case message := <-messages:
		if !strings.Contains(message, "NOT CHECKED (credential unavailable)") || calls.Load() != 0 {
			t.Fatalf("credential failure issued a request or wrong report: %q, calls=%d", message, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing credential failure report")
	}
	available.Store(true)
	select {
	case message := <-messages:
		if !strings.Contains(message, "divergence: none") {
			t.Fatalf("missing recovery: %q", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential recovery was not checked")
	}
	watcher.Stop()
	select {
	case <-watcher.done:
	default:
		t.Fatal("Stop returned before watcher finished")
	}
}

func TestWorkerDigestKeyAuthenticatesPollingAndDetectsRecovery(t *testing.T) {
	root := initDemo(t)
	configureDispatchAuthority(t, root)
	source, err := workerDigestTokenSource(root, "")
	if err != nil || source == nil {
		t.Fatalf("shared-key watcher disabled: %v", err)
	}
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	var requests, mints atomic.Int32
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(auth), httpapi.WithConfigDigest(func() string {
			if requests.Add(1) == 2 {
				return "sha256:changed"
			}
			return "sha256:original"
		}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	messages := make(chan string, 16)
	seams := &workerSeams{logf: func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }}
	seams.snapshot.Store(&workerConfigSnapshot{digest: "sha256:original"})
	watcher := startWorkerDivergenceWatcher(context.Background(), seams, server.Client(), server.URL, func() (string, error) { mints.Add(1); return source() }, 10*time.Millisecond)
	t.Cleanup(watcher.Stop)
	for _, want := range []string{"divergence: none", "daemon has sha256:changed", "divergence: none"} {
		select {
		case message := <-messages:
			if !strings.Contains(message, want) {
				t.Fatalf("report %q does not contain %q", message, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("authenticated watcher did not report divergence and recovery")
		}
	}
	watcher.Stop()
	if mints.Load() < 3 || mints.Load() != requests.Load() {
		t.Fatalf("credentials not minted per poll: %d mints, %d reads", mints.Load(), requests.Load())
	}
	// The actual same credential is refused at a human run-list route.
	token, err := source()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, apicontract.RunsPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("worker run list = %d", response.Code)
	}
}

func TestWorkerDigestTokenSourceKeepsStaticAndUnconfiguredPostures(t *testing.T) {
	root := initDemo(t)
	source, err := workerDigestTokenSource(root, "")
	if err != nil || source != nil {
		t.Fatalf("unconfigured auth: source=%v, err=%v", source != nil, err)
	}
	source, err = workerDigestTokenSource("unreadable-instance", "explicit-static-token")
	if err != nil {
		t.Fatal(err)
	}
	token, err := source()
	if err != nil || token != "explicit-static-token" {
		t.Fatal("static token compatibility lost")
	}
	if _, err := workerDigestTokenSource("unreadable-instance", ""); err == nil {
		t.Fatal("unreadable configured instance silently disables authentication")
	}
}

func TestWorkerDigestFetchRefusesRedirectAndOversizedPayload(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, `{"digest":"leaked"}`)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	if _, err := fetchDaemonConfigDigest(context.Background(), redirect.Client(), redirect.URL, "worker-secret"); err == nil || targetCalls.Load() != 0 {
		t.Fatal("worker credential followed a redirect")
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"digest":"`+strings.Repeat("x", 4096)+`"}`)
	}))
	t.Cleanup(large.Close)
	if _, err := fetchDaemonConfigDigest(context.Background(), large.Client(), large.URL, "worker-secret"); err == nil {
		t.Fatal("unbounded response accepted")
	}
}
