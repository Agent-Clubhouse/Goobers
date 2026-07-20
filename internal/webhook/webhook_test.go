package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type recordingSignaler struct {
	mu      sync.Mutex
	signals []string
}

func (s *recordingSignaler) Signal(_ context.Context, name string, _ time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, name)
	return []string{"run-1"}
}

func (s *recordingSignaler) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signals...)
}

type recordingJournal struct {
	events []journal.Event
	err    error
}

type countingReader struct {
	reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func alwaysReady() bool { return true }

func TestNewHandlerValidatesDependencies(t *testing.T) {
	signaler := &recordingSignaler{}
	instanceLog := &recordingJournal{}
	cases := []struct {
		name     string
		ctx      context.Context
		secret   []byte
		signaler Signaler
		journal  InstanceJournal
		ready    func() bool
	}{
		{name: "context", secret: []byte("secret"), signaler: signaler, journal: instanceLog, ready: alwaysReady},
		{name: "secret", ctx: context.Background(), signaler: signaler, journal: instanceLog, ready: alwaysReady},
		{name: "signaler", ctx: context.Background(), secret: []byte("secret"), journal: instanceLog, ready: alwaysReady},
		{name: "journal", ctx: context.Background(), secret: []byte("secret"), signaler: signaler, ready: alwaysReady},
		{name: "readiness", ctx: context.Background(), secret: []byte("secret"), signaler: signaler, journal: instanceLog},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHandler(tc.ctx, tc.secret, tc.signaler, tc.journal, tc.ready); err == nil {
				t.Fatal("NewHandler unexpectedly succeeded")
			}
		})
	}
}

func (l *recordingJournal) Append(event journal.Event) error {
	l.events = append(l.events, event)
	return l.err
}

func TestValidSignatureGitHubVector(t *testing.T) {
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")
	const signature = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"

	if !ValidSignature(secret, body, signature) {
		t.Fatal("known GitHub signature vector was rejected")
	}
	for _, invalid := range []string{
		"",
		"sha1=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17",
		"sha256=not-hex",
		"sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e18",
	} {
		if ValidSignature(secret, body, invalid) {
			t.Fatalf("invalid signature %q was accepted", invalid)
		}
	}
}

func TestHandlerRoutesGitHubEventNames(t *testing.T) {
	const secret = "webhook-test-secret"
	signaler := &recordingSignaler{}
	handler, err := NewHandler(context.Background(), []byte(secret), signaler, &recordingJournal{}, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}

	events := []string{"pull_request", "issues", "check_suite", "pull_request_review_comment"}
	for i, event := range events {
		response := deliver(t, handler, secret, event, event+"-delivery", []byte(`{"zen":"test"}`))
		if response.Code != http.StatusAccepted {
			t.Fatalf("event %q status = %d, want %d", event, response.Code, http.StatusAccepted)
		}
		if got := signaler.names(); len(got) != i+1 || got[i] != SignalName(event) {
			t.Fatalf("signals after %q = %v", event, got)
		}
	}
}

func TestHandlerRejectsInvalidSignatureAndJournals(t *testing.T) {
	signaler := &recordingSignaler{}
	instanceLog := &recordingJournal{}
	handler, err := NewHandler(context.Background(), []byte("right-secret"), signaler, instanceLog, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}

	response := deliver(t, handler, "wrong-secret", "issues", "delivery-1", []byte(`{}`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := signaler.names(); len(got) != 0 {
		t.Fatalf("invalid delivery signaled %v", got)
	}
	if len(instanceLog.events) != 1 ||
		instanceLog.events[0].Type != journal.EventError ||
		instanceLog.events[0].Error == nil ||
		instanceLog.events[0].Error.Code != "webhook_signature_invalid" {
		t.Fatalf("journal events = %+v", instanceLog.events)
	}
}

func TestHandlerSuppressesReplayedDelivery(t *testing.T) {
	const secret = "webhook-test-secret"
	signaler := &recordingSignaler{}
	handler, err := NewHandler(context.Background(), []byte(secret), signaler, &recordingJournal{}, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		response := deliver(t, handler, secret, "issues", "same-delivery", []byte(`{}`))
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
		}
	}
	if got := signaler.names(); len(got) != 1 {
		t.Fatalf("replayed delivery signaled %d times, want 1: %v", len(got), got)
	}
}

func TestHandlerRejectsUnsupportedRequestShapes(t *testing.T) {
	const secret = "webhook-test-secret"
	handler, err := NewHandler(context.Background(), []byte(secret), &recordingSignaler{}, &recordingJournal{}, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/other", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("wrong-path status = %d, want %d", response.Code, http.StatusNotFound)
	}

	request = httptest.NewRequest(http.MethodGet, Path, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET response = status %d Allow %q", response.Code, response.Header().Get("Allow"))
	}

	body := []byte(`{}`)
	request = httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	request.Header.Set(signatureHeader, sign(secret, body))
	request.Header.Set(deliveryHeader, "delivery-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing-event status = %d, want %d", response.Code, http.StatusBadRequest)
	}

	request = httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	request.Header.Set(signatureHeader, sign(secret, body))
	request.Header.Set(eventHeader, "issues")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing-delivery status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestHandlerRejectsDeliveriesOutsideReadyLifecycle(t *testing.T) {
	const secret = "webhook-test-secret"
	var ready atomic.Bool
	signaler := &recordingSignaler{}
	ctx, cancel := context.WithCancel(context.Background())
	handler, err := NewHandler(ctx, []byte(secret), signaler, &recordingJournal{}, ready.Load)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{}`)
	if response := deliver(t, handler, secret, "issues", "startup", body); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("startup status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	ready.Store(true)
	if response := deliver(t, handler, secret, "issues", "ready", body); response.Code != http.StatusAccepted {
		t.Fatalf("ready status = %d, want %d", response.Code, http.StatusAccepted)
	}
	ready.Store(false)
	if response := deliver(t, handler, secret, "issues", "shutdown", body); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	ready.Store(true)
	cancel()
	if response := deliver(t, handler, secret, "issues", "canceled", body); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled-context status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := signaler.names(); len(got) != 1 {
		t.Fatalf("signals = %v, want one ready-lifecycle delivery", got)
	}
}

func TestHandlerRejectsOversizedContentLengthWithoutReading(t *testing.T) {
	handler, err := NewHandler(context.Background(), []byte("secret"), &recordingSignaler{}, &recordingJournal{}, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}
	body := &countingReader{}
	request := httptest.NewRequest(http.MethodPost, Path, body)
	request.ContentLength = maxBodyBytes + 1
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	if body.reads != 0 {
		t.Fatalf("oversized body read %d time(s), want 0", body.reads)
	}
}

func TestHandlerFailsClosedWhenRejectionCannotBeJournaled(t *testing.T) {
	handler, err := NewHandler(context.Background(), []byte("right-secret"), &recordingSignaler{}, &recordingJournal{err: errors.New("disk full")}, alwaysReady)
	if err != nil {
		t.Fatal(err)
	}
	response := deliver(t, handler, "wrong-secret", "issues", "delivery-1", []byte(`{}`))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func deliver(t *testing.T, handler http.Handler, secret, event, delivery string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	request.Header.Set(signatureHeader, sign(secret, body))
	request.Header.Set(eventHeader, event)
	request.Header.Set(deliveryHeader, delivery)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
