package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

type durableTestSignaler struct {
	err        error
	deliveries []Delivery
}

func (s *durableTestSignaler) AcceptWebhook(_ context.Context, d Delivery, _ time.Time) ([]string, error) {
	s.deliveries = append(s.deliveries, d)
	return nil, s.err
}
func TestDurableWebhookRetriesAcceptanceAndFingerprintsCompletePayload(t *testing.T) {
	legacy := &recordingSignaler{}
	durable := &durableTestSignaler{err: errors.New("queue full")}
	h, err := NewHandler([]byte("secret"), legacy, &recordingJournal{}, startedDispatchGate(t, t.Context()), WithDurableSignaler(durable))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"opened","repository":{"name":"repo","owner":{"login":"org"}}}`)
	if response := deliver(t, h, "secret", "issues", "id", body); response.Code != http.StatusServiceUnavailable {
		t.Fatal(response.Code)
	}
	durable.err = nil
	if response := deliver(t, h, "secret", "issues", "id", body); response.Code != http.StatusAccepted {
		t.Fatal(response.Code)
	}
	if response := deliver(t, h, "secret", "issues", "id", []byte(`{"action":"closed","repository":{"name":"repo","owner":{"login":"org"}}}`)); response.Code != http.StatusAccepted {
		t.Fatal(response.Code)
	}
	if len(legacy.recordedDeliveries()) != 0 || len(durable.deliveries) != 3 {
		t.Fatal(legacy.recordedDeliveries(), durable.deliveries)
	}
	if durable.deliveries[0].PayloadDigest == "" || durable.deliveries[0].PayloadDigest != durable.deliveries[1].PayloadDigest || durable.deliveries[1].PayloadDigest == durable.deliveries[2].PayloadDigest {
		t.Fatal(durable.deliveries)
	}
}
