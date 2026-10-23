package webhookintake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
