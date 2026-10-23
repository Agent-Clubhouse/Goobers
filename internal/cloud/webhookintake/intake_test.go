package webhookintake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/webhook"
)

var testSecret = []byte("webhook-secret")

type fakeStore struct {
	mu       sync.Mutex
	receipts map[string]triggerqueue.SourceReceipt
	calls    int
}

func (s *fakeStore) AcceptSourceAPI(_ context.Context, batch triggerqueue.SourceBatch, now time.Time) (triggerqueue.SourceReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if prior, ok := s.receipts[batch.Key]; ok {
		if prior.Actor != batch.Actor || prior.Fingerprint != batch.Fingerprint {
			return triggerqueue.SourceReceipt{}, false, triggerqueue.ErrConflict
		}
		return prior, true, nil
	}
	receipt := triggerqueue.SourceReceipt{Key: batch.Key, Actor: batch.Actor, Fingerprint: batch.Fingerprint, AcceptedAt: now}
	for range batch.Starts {
		receipt.AcceptanceIDs = append(receipt.AcceptanceIDs, "trigger-"+hex.EncodeToString([]byte{byte(len(s.receipts)), byte(len(receipt.AcceptanceIDs))}))
	}
	if s.receipts == nil {
		s.receipts = map[string]triggerqueue.SourceReceipt{}
	}
	s.receipts[batch.Key] = receipt
	return receipt, false, nil
}

func (s *fakeStore) SourceReceipt(_ context.Context, key string) (triggerqueue.SourceReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.receipts[key]; ok {
		return r, nil
	}
	return triggerqueue.SourceReceipt{}, sql.ErrNoRows
}

type fakeSubs struct {
	subs []cloudconfig.WebhookSubscription
}

func (f fakeSubs) WebhookRecipients(event, owner, name string) (string, []cloudconfig.WebhookSubscription, error) {
	var out []cloudconfig.WebhookSubscription
	for _, s := range f.subs {
		if s.Owner == owner && s.Name == name && slices.Contains(s.Events, event) {
			out = append(out, s)
		}
	}
	return "generation-1", out, nil
}

func testIntake(store SourceStore) *Intake {
	return &Intake{
		Store: store,
		Subs: fakeSubs{subs: []cloudconfig.WebhookSubscription{
			{Gaggle: "g", Workflow: "review", Owner: "acme", Name: "app", Events: []string{"pull_request"}},
			{Gaggle: "g", Workflow: "triage", Owner: "acme", Name: "app", Events: []string{"pull_request"}},
		}},
		Pin: func(context.Context, string, string, string) (string, string, error) {
			return "wf-digest", "goober-digest", nil
		},
	}
}

const body = `{"action":"opened","repository":{"name":"app","owner":{"login":"acme"}},"number":7}`

func sign(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func deliver(t *testing.T, h http.Handler, signature, delivery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signature)
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", delivery)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newTestHandler(t *testing.T, intake *Intake) http.Handler {
	t.Helper()
	handler, stop, err := NewHandler(t.Context(), testSecret, intake, log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return handler
}

func TestInvalidSignatureIsRefusedWithoutDurableWrite(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, testIntake(store))
	for name, signature := range map[string]string{
		"wrong secret": sign([]byte("other"), body),
		"tampered":     sign(testSecret, body+" "),
		"missing":      "",
		"malformed":    "sha256=zz",
	} {
		if rec := deliver(t, handler, signature, "delivery-"+name); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, rec.Code)
		}
	}
	if store.calls != 0 || len(store.receipts) != 0 {
		t.Fatalf("store touched %d times by unauthenticated deliveries", store.calls)
	}
}

func TestSignedDeliveryIsAcceptedOnceAcrossRedelivery(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, testIntake(store))
	for range 3 {
		if rec := deliver(t, handler, sign(testSecret, body), "delivery-1"); rec.Code != http.StatusAccepted {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	if len(store.receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(store.receipts))
	}
	for _, receipt := range store.receipts {
		if len(receipt.AcceptanceIDs) != 2 {
			t.Fatalf("acceptances = %v, want one per subscribed workflow", receipt.AcceptanceIDs)
		}
	}
}

func TestUnavailablePinFailsClosed(t *testing.T) {
	store := &fakeStore{}
	intake := testIntake(store)
	intake.Pin = func(context.Context, string, string, string) (string, string, error) {
		return "", "", ErrPinUnavailable
	}
	rec := deliver(t, newTestHandler(t, intake), sign(testSecret, body), "delivery-pin")
	if rec.Code != http.StatusServiceUnavailable || store.calls != 0 {
		t.Fatalf("status %d store calls %d, want 503 and no write", rec.Code, store.calls)
	}
}

func deliverBody(t *testing.T, h http.Handler, event, payload, delivery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, payload))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDeliveryIDReuseWithDifferentPayloadIs409(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, testIntake(store))
	if rec := deliver(t, handler, sign(testSecret, body), "delivery-c"); rec.Code != http.StatusAccepted {
		t.Fatalf("first status %d", rec.Code)
	}
	other := strings.Replace(body, `"number":7`, `"number":8`, 1)
	if rec := deliverBody(t, handler, "pull_request", other, "delivery-c"); rec.Code != http.StatusConflict {
		t.Fatalf("identity conflict status %d, want 409", rec.Code)
	}
	if len(store.receipts) != 1 {
		t.Fatalf("receipts = %d, want the original only", len(store.receipts))
	}
}

func TestSignedMalformedPayloadsPersistNothing(t *testing.T) {
	store := &fakeStore{}
	handler := newTestHandler(t, testIntake(store))
	for name, tc := range map[string]struct{ event, payload string }{
		"push null":    {"push", "null"},
		"issues {}":    {"issues", "{}"},
		"push no repo": {"push", `{"repository":{}}`},
	} {
		if rec := deliverBody(t, handler, tc.event, tc.payload, "delivery-"+name); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if store.calls != 0 || len(store.receipts) != 0 {
		t.Fatalf("malformed deliveries wrote: calls=%d receipts=%d", store.calls, len(store.receipts))
	}
}

func TestEventWithNoSubscriberIsANoOpWithoutReceipt(t *testing.T) {
	store := &fakeStore{}
	intake := testIntake(store)
	handler := newTestHandler(t, intake)
	push := `{"repository":{"name":"app","owner":{"login":"acme"}}}`
	if rec := deliverBody(t, handler, "push", push, "delivery-n"); rec.Code != http.StatusAccepted {
		t.Fatalf("no-subscriber status %d, want 202", rec.Code)
	}
	if len(store.receipts) != 0 {
		t.Fatal("an unsubscribed event reserved the delivery ID with an empty receipt")
	}
	// After a configuration that subscribes to it, the same delivery ID is accepted.
	intake.Subs = fakeSubs{subs: []cloudconfig.WebhookSubscription{{Gaggle: "g", Workflow: "pushed", Owner: "acme", Name: "app", Events: []string{"push"}}}}
	if rec := deliverBody(t, handler, "push", push, "delivery-n"); rec.Code != http.StatusAccepted || len(store.receipts) != 1 {
		t.Fatalf("later subscribed redelivery: status %d receipts %d", rec.Code, len(store.receipts))
	}
}

func TestRedeliveryAnswersFromStoredReceiptWithoutPinning(t *testing.T) {
	store := &fakeStore{}
	intake := testIntake(store)
	first, err := intake.AcceptWebhook(t.Context(), webhook.Delivery{Event: "pull_request", ID: "delivery-e", RepositoryOwner: "acme", RepositoryName: "app", PullNumber: 7, PayloadDigest: "d"}, time.Now())
	if err != nil || len(first) != 2 {
		t.Fatalf("first = %v, %v", first, err)
	}
	intake.Pin = func(context.Context, string, string, string) (string, string, error) {
		return "", "", ErrPinUnavailable
	}
	again, err := intake.AcceptWebhook(t.Context(), webhook.Delivery{Event: "pull_request", ID: "delivery-e", RepositoryOwner: "acme", RepositoryName: "app", PullNumber: 7, PayloadDigest: "d"}, time.Now())
	if err != nil || !slices.Equal(first, again) {
		t.Fatalf("redelivery = %v, %v; want the stored IDs %v", again, err, first)
	}
	if _, err := intake.AcceptWebhook(t.Context(), webhook.Delivery{Event: "pull_request", ID: "delivery-fresh", RepositoryOwner: "acme", RepositoryName: "app", PullNumber: 7, PayloadDigest: "d"}, time.Now()); !errors.Is(err, ErrPinUnavailable) {
		t.Fatalf("a fresh delivery without pinning = %v, want fail closed", err)
	}
}
